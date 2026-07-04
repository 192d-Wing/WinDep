import { useCallback, useEffect, useState } from "react";
import AppLayout from "@cloudscape-design/components/app-layout";
import ContentLayout from "@cloudscape-design/components/content-layout";
import Container from "@cloudscape-design/components/container";
import Header from "@cloudscape-design/components/header";
import Table from "@cloudscape-design/components/table";
import Button from "@cloudscape-design/components/button";
import SpaceBetween from "@cloudscape-design/components/space-between";
import FileUpload from "@cloudscape-design/components/file-upload";
import Select from "@cloudscape-design/components/select";
import Box from "@cloudscape-design/components/box";
import Alert from "@cloudscape-design/components/alert";

interface FileInfo {
  name: string;
  size: number;
  modified: string;
}

const CATEGORIES = [
  { label: "images (WIMs)", value: "images" },
  { label: "config", value: "config" },
  { label: "boot", value: "boot" },
];

function humanSize(n: number): string {
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${u[i]}`;
}

export default function App() {
  const [category, setCategory] = useState(CATEGORIES[0]);
  const [items, setItems] = useState<FileInfo[]>([]);
  const [selected, setSelected] = useState<FileInfo[]>([]);
  const [upload, setUpload] = useState<File[]>([]);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const cat = category.value;

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const r = await fetch(`/api/files?category=${cat}`);
      if (!r.ok) throw new Error(`list failed (${r.status})`);
      setItems(await r.json());
      setSelected([]);
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, [cat]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  async function doUpload() {
    setBusy(true);
    setError(null);
    try {
      for (const file of upload) {
        const r = await fetch(`/api/files/${cat}/${encodeURIComponent(file.name)}`, {
          method: "PUT",
          body: file,
        });
        if (!r.ok) throw new Error(`upload ${file.name} failed (${r.status})`);
      }
      setUpload([]);
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
        const r = await fetch(`/api/files/${cat}/${encodeURIComponent(f.name)}`, {
          method: "DELETE",
        });
        if (!r.ok) throw new Error(`delete ${f.name} failed (${r.status})`);
      }
      await refresh();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <AppLayout
      navigationHide
      toolsHide
      content={
        <ContentLayout header={<Header variant="h1" description="Upload and remove deploy payloads on the WinDep PV">WinDep Deploy — Admin</Header>}>
          <SpaceBetween size="l">
            {error && <Alert type="error" dismissible onDismiss={() => setError(null)} header="Error">{error}</Alert>}

            <Container header={<Header variant="h2">Upload</Header>}>
              <SpaceBetween size="m">
                <Select
                  selectedOption={category}
                  onChange={(e) => setCategory(e.detail.selectedOption as typeof CATEGORIES[number])}
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
                  constraintText="WIMs go to images; JSON to config; boot files to boot."
                />
                <Button variant="primary" loading={busy} disabled={upload.length === 0} onClick={doUpload}>
                  Upload {upload.length > 0 ? `${upload.length} file(s)` : ""}
                </Button>
              </SpaceBetween>
            </Container>

            <Table<FileInfo>
              items={items}
              loading={loading}
              loadingText="Loading files"
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
                      <Button iconName="refresh" loading={loading} onClick={() => void refresh()}>Refresh</Button>
                      <Button loading={busy} disabled={selected.length === 0} onClick={doDelete}>
                        Delete{selected.length > 0 ? ` (${selected.length})` : ""}
                      </Button>
                    </SpaceBetween>
                  }
                >
                  {category.label}
                </Header>
              }
              columnDefinitions={[
                { id: "name", header: "Name", cell: (f) => f.name, sortingField: "name", isRowHeader: true },
                { id: "size", header: "Size", cell: (f) => humanSize(f.size), sortingField: "size" },
                { id: "modified", header: "Modified", cell: (f) => f.modified, sortingField: "modified" },
              ]}
              empty={<Box textAlign="center" color="inherit"><b>No files</b><Box variant="p" color="inherit">Upload one above.</Box></Box>}
            />
          </SpaceBetween>
        </ContentLayout>
      }
    />
  );
}
