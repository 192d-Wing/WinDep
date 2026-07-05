package main

import (
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf16"
)

// A WIM file embeds its image catalogue as a UTF-16LE XML blob whose location is
// recorded in the 208-byte header. Parsing it here gives the same edition / build /
// index detail DISM prints, without shelling out to DISM — which the Linux admin
// container does not have. Reference: [MS-WIM] WIMHEADER_V1_PACKED / RESHDR_DISK_SHORT.

// WIMImage is the per-index metadata for one image inside a WIM.
type WIMImage struct {
	Index   int    `json:"index"`
	Name    string `json:"name"`
	Edition string `json:"edition"`
	Arch    string `json:"arch"`
	Build   string `json:"build"` // MAJOR.MINOR.BUILD.SPBUILD
	Size    int64  `json:"size"`  // uncompressed TOTALBYTES for the image
}

const (
	wimMagic         = "MSWIM\x00\x00\x00"
	wimHeaderLen     = 208
	xmlReshdrOff     = 72     // byte offset of rhXmlData within the header
	reshdrCompressed = 0x04   // RESHDR flag: resource is LZX/XPRESS compressed
	xmlSizeCap       = 64 << 20 // sanity bound on the XML metadata blob
)

var errNotWIM = errors.New("not a WIM file")

// wimImages parses the WIM at path and returns its per-index image metadata.
func wimImages(path string) ([]WIMImage, error) {
	blob, err := wimXML(path)
	if err != nil {
		return nil, err
	}
	var root struct {
		Images []struct {
			Index   int    `xml:"INDEX,attr"`
			Name    string `xml:"NAME"`
			Total   int64  `xml:"TOTALBYTES"`
			Windows struct {
				Arch      int    `xml:"ARCH"`
				EditionID string `xml:"EDITIONID"`
				Version   struct {
					Major   int `xml:"MAJOR"`
					Minor   int `xml:"MINOR"`
					Build   int `xml:"BUILD"`
					SPBuild int `xml:"SPBUILD"`
				} `xml:"VERSION"`
			} `xml:"WINDOWS"`
		} `xml:"IMAGE"`
	}
	if err := xml.Unmarshal([]byte(blob), &root); err != nil {
		return nil, errNotWIM
	}
	out := make([]WIMImage, 0, len(root.Images))
	for _, im := range root.Images {
		v := im.Windows.Version
		out = append(out, WIMImage{
			Index:   im.Index,
			Name:    im.Name,
			Edition: im.Windows.EditionID,
			Arch:    archName(im.Windows.Arch),
			Build:   fmt.Sprintf("%d.%d.%d.%d", v.Major, v.Minor, v.Build, v.SPBuild),
			Size:    im.Total,
		})
	}
	return out, nil
}

// wimXML reads the WIM's embedded XML metadata resource and decodes it to UTF-8.
// Only the header and the (small) XML blob are read; the multi-GB body is seeked
// past, so this stays cheap regardless of image size.
func wimXML(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hdr := make([]byte, wimHeaderLen)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return "", errNotWIM
	}
	if string(hdr[:8]) != wimMagic {
		return "", errNotWIM
	}
	// rhXmlData is a RESHDR: [7-byte size | 1-byte flags][8-byte offset][8-byte original size].
	rh := hdr[xmlReshdrOff:]
	sizeFlags := binary.LittleEndian.Uint64(rh[0:8])
	size := sizeFlags & 0x00FFFFFFFFFFFFFF
	flags := byte(sizeFlags >> 56)
	offset := binary.LittleEndian.Uint64(rh[8:16])
	if flags&reshdrCompressed != 0 {
		return "", errors.New("WIM XML metadata is compressed (unsupported)")
	}
	if size == 0 || size > xmlSizeCap {
		return "", errNotWIM
	}
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, int64(offset)); err != nil {
		return "", err
	}
	return decodeUTF16LE(buf), nil
}

// decodeUTF16LE turns a little-endian UTF-16 byte slice (WIM XML is stored this way,
// with a leading BOM) into a Go UTF-8 string.
func decodeUTF16LE(b []byte) string {
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE { // strip BOM
		b = b[2:]
	}
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u16 = append(u16, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(u16))
}

// archName maps a WIM PROCESSOR_ARCHITECTURE code to a human label.
func archName(a int) string {
	switch a {
	case 0:
		return "x86"
	case 5:
		return "ARM"
	case 6:
		return "IA64"
	case 9:
		return "x64"
	case 12:
		return "ARM64"
	default:
		return fmt.Sprintf("arch-%d", a)
	}
}
