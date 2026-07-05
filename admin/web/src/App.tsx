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
import BreadcrumbGroup from "@cloudscape-design/components/breadcrumb-group";
import Modal from "@cloudscape-design/components/modal";
import Input from "@cloudscape-design/components/input";
import ProgressBar from "@cloudscape-design/components/progress-bar";
import Link from "@cloudscape-design/components/link";
import Icon from "@cloudscape-design/components/icon";

interface FileInfo {
  name: string;
  size: number;
  modified: string;
  isDir: boolean;
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

// join a category + prefix + optional leaf into an /api path with each segment encoded.
function apiPath(base: string, category: string, prefix: string, leaf?: string): string {
  const segs = [category, ...prefix.split("/").filter(Boolean)];
  if (leaf) segs.push(leaf);
  return `/api/${base}/${segs.map(encodeURIComponent).join("/")}`;
}

// PUT a File with real upload progress (fetch cannot report upload progress; XHR can).
function putWithProgress(url: string, file: File, onPct: (pct: number) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", url);
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onPct(Math.round((e.loaded / e.total) * 100));
    };
    xhr.onload = () =>
      xhr.status >= 200 && xhr.status < 300
        ? resolve()
        : reject(new Error(`${file.name}: HTTP ${xhr.status}`));
    xhr.onerror = () => reject(new Error(`${file.name}: network error`));
    xhr.send(file);
  });
}

export default function App() {
  const [category, setCategory] = useState(CATEGORIES[0]);
  const [prefix, setPrefix] = useState("");
  const [items, setItems] = useState<FileInfo[]>([]);
  const [selected, setSelected] = useState<FileInfo[]>([]);
  const [upload, setUpload] = useState<File[]>([]);
  const [progress, setProgress] = useState<Record<string, number>>({});
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [newFolder, setNewFolder] = useState<string | null>(null); // modal open when non-null

  const cat = category.value;

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const q = new URLSearchParams({ category: cat, prefix });
      const r = await fetch(`/api/files?${q}`);
      if (!r.ok) throw new Error(`list failed (${r.status})`);
      const data: FileInfo[] = await r.json();
      // folders first, then files, each alphabetical
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

  // reset to the category root when the category changes
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

  // breadcrumb: category root + each prefix segment
  const segs = prefix.split("/").filter(Boolean);
  const crumbs = [
    { text: category.label, href: "" },
    ...segs.map((s, i) => ({ text: s, href: segs.slice(0, i + 1).join("/") })),
  ];

  return (
    <AppLayout
      navigationHide
      toolsHide
      content={
        <ContentLayout
          header={
            <Header variant="h1" description="Browse, upload, and manage deploy payloads on the WinDep PV">
              WinDep Deploy — Admin
            </Header>
          }
        >
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
                    <ProgressBar
                      key={name}
                      value={pct}
                      label={name}
                      description={pct >= 100 ? "Finalizing…" : "Uploading"}
                    />
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
                      <Button variant="inline-icon" iconName="download" ariaLabel={`Download ${f.name}`} onClick={() => download(f.name)} />
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
          </SpaceBetween>

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
            <Input
              value={newFolder ?? ""}
              onChange={(e) => setNewFolder(e.detail.value)}
              placeholder="folder name"
              autoFocus
            />
          </Modal>
        </ContentLayout>
      }
    />
  );
}
