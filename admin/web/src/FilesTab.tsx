import { useCallback, useEffect, useState } from "react";
import Container from "@cloudscape-design/components/container";
import Header from "@cloudscape-design/components/header";
import Table from "@cloudscape-design/components/table";
import Button from "@cloudscape-design/components/button";
import SpaceBetween from "@cloudscape-design/components/space-between";
import FileUpload from "@cloudscape-design/components/file-upload";
import Select from "@cloudscape-design/components/select";
import Box from "@cloudscape-design/components/box";
import Alert from "@cloudscape-design/components/alert";
import BreadcrumbGroup from "@cloudscape-design/components/breadcrumb-group";
import Modal from "@cloudscape-design/components/modal";
import Input from "@cloudscape-design/components/input";
import ProgressBar from "@cloudscape-design/components/progress-bar";
import Link from "@cloudscape-design/components/link";
import Icon from "@cloudscape-design/components/icon";
import StatusIndicator from "@cloudscape-design/components/status-indicator";
import Spinner from "@cloudscape-design/components/spinner";
import CopyToClipboard from "@cloudscape-design/components/copy-to-clipboard";
import { humanSize, apiPath } from "./util";

interface FileInfo {
  name: string;
  size: number;
  modified: string;
  isDir: boolean;
  sha256?: string;
}

interface WimImage {
  index: number;
  name: string;
  edition: string;
  arch: string;
  build: string;
  size: number;
}

const CATEGORIES = [
  { label: "images (WIMs)", value: "images" },
  { label: "config", value: "config" },
  { label: "boot", value: "boot" },
];

// PUT a File with real upload progress (fetch cannot report upload progress; XHR can).
function putWithProgress(url: string, file: File, onPct: (pct: number) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", url);
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onPct(Math.round((e.loaded / e.total) * 100));
    };
    xhr.onload = () =>
      xhr.status >= 200 && xhr.status < 300 ? resolve() : reject(new Error(`${file.name}: HTTP ${xhr.status}`));
    xhr.onerror = () => reject(new Error(`${file.name}: network error`));
    xhr.send(file);
  });
}

export default function FilesTab() {
  const [category, setCategory] = useState(CATEGORIES[0]);
  const [prefix, setPrefix] = useState("");
  const [items, setItems] = useState<FileInfo[]>([]);
  const [selected, setSelected] = useState<FileInfo[]>([]);
  const [upload, setUpload] = useState<File[]>([]);
  const [progress, setProgress] = useState<Record<string, number>>({});
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [newFolder, setNewFolder] = useState<string | null>(null);
  const [detail, setDetail] = useState<FileInfo | null>(null);
  const [wim, setWim] = useState<{ images: WimImage[]; sha256: string } | null>(null);
  const [wimErr, setWimErr] = useState<string | null>(null);
  const [wimLoading, setWimLoading] = useState(false);
  const [verifying, setVerifying] = useState(false);
  const [verifyRes, setVerifyRes] = useState<{ ok: boolean; expected: string; actual: string } | null>(null);
  const [verifyErr, setVerifyErr] = useState<string | null>(null);

  const cat = category.value;

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const q = new URLSearchParams({ category: cat, prefix });
      const r = await fetch(`/api/files?${q}`);
      if (!r.ok) throw new Error(`list failed (${r.status})`);
      const data: FileInfo[] = await r.json();
      data.sort((a, b) => (a.isDir === b.isDir ? a.name.localeCompare(b.name) : a.isDir ? -1 : 1));
      setItems(data);
      setSelected([]);
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, [cat, prefix]);

  useEffect(() => {
    void refresh();
  }, [refresh]);
  useEffect(() => {
    setPrefix("");
  }, [cat]);

  async function doUpload() {
    setBusy(true);
    setError(null);
    setProgress(Object.fromEntries(upload.map((f) => [f.name, 0])));
    try {
      for (const file of upload) {
        await putWithProgress(apiPath("files", cat, prefix, file.name), file, (pct) =>
          setProgress((p) => ({ ...p, [file.name]: pct })),
        );
      }
      setUpload([]);
      setProgress({});
      await refresh();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  async function doDelete() {
    setBusy(true);
    setError(null);
    try {
      for (const f of selected) {
        const r = await fetch(apiPath("files", cat, prefix, f.name), { method: "DELETE" });
        if (!r.ok) throw new Error(`delete ${f.name} failed (${r.status})`);
      }
      await refresh();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  async function doCreateFolder() {
    const name = (newFolder ?? "").trim();
    if (!name) return;
    setBusy(true);
    setError(null);
    try {
      const r = await fetch(apiPath("folders", cat, prefix, name), { method: "POST" });
      if (!r.ok) throw new Error(`create folder failed (${r.status})`);
      setNewFolder(null);
      await refresh();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  function download(name: string) {
    const a = document.createElement("a");
    a.href = apiPath("download", cat, prefix, name);
    a.download = name;
    a.click();
  }

  async function openDetails(f: FileInfo) {
    setDetail(f);
    setWim(null);
    setWimErr(null);
    setVerifyRes(null);
    setVerifyErr(null);
    setWimLoading(true);
    try {
      const r = await fetch(apiPath("wiminfo", cat, prefix, f.name));
      if (r.ok) setWim(await r.json());
      else if (r.status === 422) setWimErr("Not a WIM image — no capture catalogue to read.");
      else setWimErr(`metadata failed (${r.status})`);
    } catch (e) {
      setWimErr(String(e));
    } finally {
      setWimLoading(false);
    }
  }

  async function doVerify() {
    if (!detail) return;
    setVerifying(true);
    setVerifyRes(null);
    setVerifyErr(null);
    try {
      const r = await fetch(apiPath("verify", cat, prefix, detail.name), { method: "POST" });
      if (!r.ok) throw new Error(`verify failed (${r.status})`);
      setVerifyRes(await r.json());
    } catch (e) {
      setVerifyErr(String(e));
    } finally {
      setVerifying(false);
    }
  }

  const segs = prefix.split("/").filter(Boolean);
  const crumbs = [
    { text: category.label, href: "" },
    ...segs.map((s, i) => ({ text: s, href: segs.slice(0, i + 1).join("/") })),
  ];

  return (
    <SpaceBetween size="l">
      {error && (
        <Alert type="error" dismissible onDismiss={() => setError(null)} header="Error">
          {error}
        </Alert>
      )}

      <Container header={<Header variant="h2">Upload</Header>}>
        <SpaceBetween size="m">
          <Select
            selectedOption={category}
            onChange={(e) => setCategory(e.detail.selectedOption as (typeof CATEGORIES)[number])}
            options={CATEGORIES}
          />
          <FileUpload
            multiple
            value={upload}
            onChange={(e) => setUpload(e.detail.value)}
            i18nStrings={{
              uploadButtonText: (multi) => (multi ? "Choose files" : "Choose file"),
              dropzoneText: (multi) => (multi ? "Drop files to upload" : "Drop file to upload"),
              removeFileAriaLabel: (i) => `Remove file ${i + 1}`,
              limitShowFewer: "Show fewer",
              limitShowMore: "Show more",
              errorIconAriaLabel: "Error",
            }}
            showFileSize
            constraintText={`Uploads land in ${category.label}${prefix ? " / " + prefix : ""}.`}
          />
          {busy &&
            Object.entries(progress).map(([name, pct]) => (
              <ProgressBar key={name} value={pct} label={name} description={pct >= 100 ? "Finalizing…" : "Uploading"} />
            ))}
          <Button variant="primary" loading={busy} disabled={upload.length === 0} onClick={doUpload}>
            Upload{upload.length > 0 ? ` ${upload.length} file(s)` : ""}
          </Button>
        </SpaceBetween>
      </Container>

      <Table<FileInfo>
        items={items}
        loading={loading}
        loadingText="Loading"
        selectionType="multi"
        selectedItems={selected}
        onSelectionChange={(e) => setSelected(e.detail.selectedItems)}
        trackBy="name"
        variant="container"
        header={
          <Header
            variant="h2"
            counter={`(${items.length})`}
            actions={
              <SpaceBetween direction="horizontal" size="xs">
                <Button iconName="folder" onClick={() => setNewFolder("")}>
                  Create folder
                </Button>
                <Button iconName="refresh" loading={loading} onClick={() => void refresh()}>
                  Refresh
                </Button>
                <Button loading={busy} disabled={selected.length === 0} onClick={doDelete}>
                  Delete{selected.length > 0 ? ` (${selected.length})` : ""}
                </Button>
              </SpaceBetween>
            }
          >
            <BreadcrumbGroup
              items={crumbs}
              onClick={(e) => {
                e.preventDefault();
                setPrefix(e.detail.href);
              }}
            />
          </Header>
        }
        columnDefinitions={[
          {
            id: "name",
            header: "Name",
            isRowHeader: true,
            cell: (f) =>
              f.isDir ? (
                <Link
                  onFollow={(e) => {
                    e.preventDefault();
                    setPrefix(prefix ? `${prefix}/${f.name}` : f.name);
                  }}
                >
                  <Icon name="folder" /> {f.name}
                </Link>
              ) : (
                <span>
                  <Icon name="file" /> {f.name}
                </span>
              ),
          },
          { id: "size", header: "Size", cell: (f) => (f.isDir ? "—" : humanSize(f.size)) },
          { id: "modified", header: "Modified", cell: (f) => f.modified },
          {
            id: "actions",
            header: "",
            cell: (f) =>
              f.isDir ? (
                ""
              ) : (
                <SpaceBetween direction="horizontal" size="xxs">
                  <Button
                    variant="inline-icon"
                    iconName="status-info"
                    ariaLabel={`Details for ${f.name}`}
                    onClick={() => void openDetails(f)}
                  />
                  <Button variant="inline-icon" iconName="download" ariaLabel={`Download ${f.name}`} onClick={() => download(f.name)} />
                </SpaceBetween>
              ),
          },
        ]}
        empty={
          <Box textAlign="center" color="inherit">
            <b>Empty</b>
            <Box variant="p" color="inherit">
              Upload a file or create a folder.
            </Box>
          </Box>
        }
      />

      <Modal
        visible={newFolder !== null}
        onDismiss={() => setNewFolder(null)}
        header="Create folder"
        footer={
          <Box float="right">
            <SpaceBetween direction="horizontal" size="xs">
              <Button variant="link" onClick={() => setNewFolder(null)}>
                Cancel
              </Button>
              <Button variant="primary" loading={busy} onClick={doCreateFolder}>
                Create
              </Button>
            </SpaceBetween>
          </Box>
        }
      >
        <Input value={newFolder ?? ""} onChange={(e) => setNewFolder(e.detail.value)} placeholder="folder name" autoFocus />
      </Modal>

      <Modal
        visible={detail !== null}
        onDismiss={() => setDetail(null)}
        size="large"
        header={detail ? `Details — ${detail.name}` : "Details"}
        footer={
          <Box float="right">
            <Button variant="link" onClick={() => setDetail(null)}>
              Close
            </Button>
          </Box>
        }
      >
        {detail && (
          <SpaceBetween size="l">
            <Container header={<Header variant="h3">Integrity</Header>}>
              <SpaceBetween size="s">
                {detail.sha256 ? (
                  <Box>
                    <Box variant="awsui-key-label">SHA-256 (recorded at upload)</Box>
                    <CopyToClipboard
                      variant="inline"
                      textToCopy={detail.sha256}
                      copyButtonAriaLabel="Copy SHA-256"
                      copySuccessText="Copied"
                      copyErrorText="Copy failed"
                    />
                  </Box>
                ) : (
                  <Box color="text-status-inactive">No checksum recorded (uploaded before integrity tracking).</Box>
                )}
                <SpaceBetween direction="horizontal" size="xs" alignItems="center">
                  <Button loading={verifying} disabled={!detail.sha256} onClick={doVerify}>
                    Verify before serve
                  </Button>
                  {verifyErr && <StatusIndicator type="error">{verifyErr}</StatusIndicator>}
                  {verifyRes &&
                    (verifyRes.ok ? (
                      <StatusIndicator type="success">Matches recorded checksum</StatusIndicator>
                    ) : (
                      <StatusIndicator type="error">Mismatch — on-disk bytes differ from upload</StatusIndicator>
                    ))}
                </SpaceBetween>
                {verifyRes && !verifyRes.ok && (
                  <Box variant="small" color="text-status-inactive">
                    expected {verifyRes.expected.slice(0, 16)}… · actual {verifyRes.actual.slice(0, 16)}…
                  </Box>
                )}
              </SpaceBetween>
            </Container>

            <Container header={<Header variant="h3">WIM images</Header>}>
              {wimLoading ? (
                <Box textAlign="center">
                  <Spinner /> Reading catalogue…
                </Box>
              ) : wimErr ? (
                <Box color="text-status-inactive">{wimErr}</Box>
              ) : (
                <Table<WimImage>
                  items={wim?.images ?? []}
                  variant="embedded"
                  columnDefinitions={[
                    { id: "index", header: "#", cell: (i) => i.index },
                    { id: "name", header: "Name", isRowHeader: true, cell: (i) => i.name },
                    { id: "edition", header: "Edition", cell: (i) => i.edition || "—" },
                    { id: "arch", header: "Arch", cell: (i) => i.arch },
                    { id: "build", header: "Build", cell: (i) => i.build },
                    { id: "size", header: "Apparent size", cell: (i) => (i.size ? humanSize(i.size) : "—") },
                  ]}
                  empty={<Box color="inherit">No images.</Box>}
                />
              )}
            </Container>
          </SpaceBetween>
        )}
      </Modal>
    </SpaceBetween>
  );
}
