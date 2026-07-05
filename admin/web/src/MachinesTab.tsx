import { useCallback, useEffect, useState } from "react";
import Table from "@cloudscape-design/components/table";
import Header from "@cloudscape-design/components/header";
import Button from "@cloudscape-design/components/button";
import SpaceBetween from "@cloudscape-design/components/space-between";
import Box from "@cloudscape-design/components/box";
import Alert from "@cloudscape-design/components/alert";
import Modal from "@cloudscape-design/components/modal";
import FormField from "@cloudscape-design/components/form-field";
import Input from "@cloudscape-design/components/input";
import Select from "@cloudscape-design/components/select";
import Toggle from "@cloudscape-design/components/toggle";
import ColumnLayout from "@cloudscape-design/components/column-layout";
import { apiPath } from "./util";

// A per-machine config is a sparse override of default.json: any field left blank
// here is omitted from the saved JSON so it inherits the default.
interface Fields {
  serial: string;
  computerName: string;
  mode: string;
  targetDisk: string;
  imageUrl: string;
  confirmWipe: boolean;
  TIMEZONE: string;
  LOCALADMINUSER: string;
  LOCALADMINPASS: string;
  JOINDOMAIN: string;
  DOMAINOU: string;
  DOMAINUSER: string;
  DOMAINPASS: string;
}

const EMPTY: Fields = {
  serial: "",
  computerName: "",
  mode: "zerotouch",
  targetDisk: "first",
  imageUrl: "",
  confirmWipe: false,
  TIMEZONE: "",
  LOCALADMINUSER: "",
  LOCALADMINPASS: "",
  JOINDOMAIN: "",
  DOMAINOU: "",
  DOMAINUSER: "",
  DOMAINPASS: "",
};

const MODES = [
  { label: "Zero-touch", value: "zerotouch" },
  { label: "Interactive", value: "interactive" },
];

interface Row {
  serial: string;
  computerName: string;
}

const MACHINES_PREFIX = "machines";

// build the sparse override JSON, omitting empty fields so they inherit default.json.
function toConfig(f: Fields): Record<string, unknown> {
  const unattend: Record<string, string> = {};
  for (const k of ["TIMEZONE", "LOCALADMINUSER", "LOCALADMINPASS", "JOINDOMAIN", "DOMAINOU", "DOMAINUSER", "DOMAINPASS"] as const) {
    if (f[k].trim()) unattend[k] = f[k].trim();
  }
  const cfg: Record<string, unknown> = {};
  if (f.mode) cfg.mode = f.mode;
  if (f.targetDisk.trim()) cfg.targetDisk = f.targetDisk.trim();
  if (f.computerName.trim()) cfg.computerName = f.computerName.trim();
  if (f.imageUrl.trim()) cfg.imageUrl = f.imageUrl.trim();
  cfg.confirmWipe = f.confirmWipe;
  if (Object.keys(unattend).length) cfg.unattend = unattend;
  return cfg;
}

function fromConfig(serial: string, c: Record<string, any>): Fields {
  const u = c.unattend ?? {};
  return {
    ...EMPTY,
    serial,
    computerName: c.computerName ?? "",
    mode: c.mode ?? "zerotouch",
    targetDisk: c.targetDisk ?? "",
    imageUrl: c.imageUrl ?? "",
    confirmWipe: Boolean(c.confirmWipe),
    TIMEZONE: u.TIMEZONE ?? "",
    LOCALADMINUSER: u.LOCALADMINUSER ?? "",
    LOCALADMINPASS: u.LOCALADMINPASS ?? "",
    JOINDOMAIN: u.JOINDOMAIN ?? "",
    DOMAINOU: u.DOMAINOU ?? "",
    DOMAINUSER: u.DOMAINUSER ?? "",
    DOMAINPASS: u.DOMAINPASS ?? "",
  };
}

export default function MachinesTab() {
  const [rows, setRows] = useState<Row[]>([]);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<Fields | null>(null); // non-null = modal open
  const [isNew, setIsNew] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const r = await fetch(`/api/files?category=config&prefix=${MACHINES_PREFIX}`);
      if (!r.ok) throw new Error(`list failed (${r.status})`);
      const files: { name: string; isDir: boolean }[] = await r.json();
      const jsons = files.filter((f) => !f.isDir && f.name.endsWith(".json"));
      // fetch each to show its computerName (small files)
      const out: Row[] = await Promise.all(
        jsons.map(async (f) => {
          const serial = f.name.replace(/\.json$/, "");
          try {
            const cr = await fetch(apiPath("download", "config", `${MACHINES_PREFIX}/${f.name}`));
            const c = cr.ok ? await cr.json() : {};
            return { serial, computerName: c.computerName ?? "" };
          } catch {
            return { serial, computerName: "" };
          }
        }),
      );
      setRows(out.sort((a, b) => a.serial.localeCompare(b.serial)));
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  async function openEdit(serial: string) {
    setError(null);
    try {
      const r = await fetch(apiPath("download", "config", `${MACHINES_PREFIX}/${serial}.json`));
      if (!r.ok) throw new Error(`load failed (${r.status})`);
      setEditing(fromConfig(serial, await r.json()));
      setIsNew(false);
    } catch (e) {
      setError(String(e));
    }
  }

  function openNew() {
    setEditing({ ...EMPTY });
    setIsNew(true);
  }

  async function save() {
    if (!editing) return;
    const serial = editing.serial.trim();
    if (!serial || /[\\/]/.test(serial)) {
      setError("Serial is required and cannot contain slashes.");
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const body = JSON.stringify(toConfig(editing), null, 2);
      const r = await fetch(apiPath("files", "config", `${MACHINES_PREFIX}/${serial}.json`), {
        method: "PUT",
        body,
      });
      if (!r.ok) throw new Error(`save failed (${r.status})`);
      setEditing(null);
      await refresh();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  async function remove(serial: string) {
    setBusy(true);
    setError(null);
    try {
      const r = await fetch(apiPath("files", "config", `${MACHINES_PREFIX}/${serial}.json`), { method: "DELETE" });
      if (!r.ok) throw new Error(`delete failed (${r.status})`);
      await refresh();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  const set = (patch: Partial<Fields>) => setEditing((e) => (e ? { ...e, ...patch } : e));

  return (
    <SpaceBetween size="l">
      {error && (
        <Alert type="error" dismissible onDismiss={() => setError(null)}>
          {error}
        </Alert>
      )}
      <Table<Row>
        items={rows}
        loading={loading}
        loadingText="Loading machine configs"
        variant="container"
        trackBy="serial"
        header={
          <Header
            variant="h2"
            counter={`(${rows.length})`}
            description="Per-machine ZTP overrides — config/machines/<serial>.json. Blank fields inherit default.json."
            actions={
              <SpaceBetween direction="horizontal" size="xs">
                <Button iconName="refresh" loading={loading} onClick={() => void refresh()}>
                  Refresh
                </Button>
                <Button variant="primary" iconName="add-plus" onClick={openNew}>
                  New machine
                </Button>
              </SpaceBetween>
            }
          >
            Machines
          </Header>
        }
        columnDefinitions={[
          { id: "serial", header: "Serial", cell: (r) => r.serial, isRowHeader: true },
          { id: "computerName", header: "Computer name", cell: (r) => r.computerName || "—" },
          {
            id: "actions",
            header: "",
            cell: (r) => (
              <SpaceBetween direction="horizontal" size="xs">
                <Button variant="inline-link" onClick={() => void openEdit(r.serial)}>
                  Edit
                </Button>
                <Button variant="inline-link" onClick={() => void remove(r.serial)}>
                  Delete
                </Button>
              </SpaceBetween>
            ),
          },
        ]}
        empty={
          <Box textAlign="center" color="inherit">
            <b>No per-machine configs</b>
            <Box variant="p" color="inherit">
              Create one to override default.json for a specific machine.
            </Box>
          </Box>
        }
      />

      <Modal
        visible={editing !== null}
        onDismiss={() => setEditing(null)}
        size="large"
        header={isNew ? "New machine config" : `Edit ${editing?.serial}`}
        footer={
          <Box float="right">
            <SpaceBetween direction="horizontal" size="xs">
              <Button variant="link" onClick={() => setEditing(null)}>
                Cancel
              </Button>
              <Button variant="primary" loading={busy} onClick={() => void save()}>
                Save
              </Button>
            </SpaceBetween>
          </Box>
        }
      >
        {editing && (
          <SpaceBetween size="m">
            <ColumnLayout columns={2}>
              <FormField label="Serial" description="Sanitized BIOS serial; the file name.">
                <Input value={editing.serial} disabled={!isNew} onChange={(e) => set({ serial: e.detail.value })} placeholder="5CG1234ABC" />
              </FormField>
              <FormField label="Computer name">
                <Input value={editing.computerName} onChange={(e) => set({ computerName: e.detail.value })} placeholder="ENG-DEV-07" />
              </FormField>
              <FormField label="Mode">
                <Select
                  selectedOption={MODES.find((m) => m.value === editing.mode) ?? MODES[0]}
                  onChange={(e) => set({ mode: e.detail.selectedOption.value! })}
                  options={MODES}
                />
              </FormField>
              <FormField label="Target disk" description="first | largest | disk number">
                <Input value={editing.targetDisk} onChange={(e) => set({ targetDisk: e.detail.value })} placeholder="first" />
              </FormField>
              <FormField label="Image URL" description="Blank = inherit default.json">
                <Input value={editing.imageUrl} onChange={(e) => set({ imageUrl: e.detail.value })} placeholder="https://deploy.oopl.dev.mil/images/install.wim" />
              </FormField>
              <FormField label="Confirm wipe">
                <Toggle checked={editing.confirmWipe} onChange={(e) => set({ confirmWipe: e.detail.checked })}>
                  Require confirmation before wiping
                </Toggle>
              </FormField>
            </ColumnLayout>

            <Header variant="h3">Unattend (blank inherits default)</Header>
            <ColumnLayout columns={2}>
              <FormField label="Timezone">
                <Input value={editing.TIMEZONE} onChange={(e) => set({ TIMEZONE: e.detail.value })} placeholder="Central Standard Time" />
              </FormField>
              <FormField label="Local admin user">
                <Input value={editing.LOCALADMINUSER} onChange={(e) => set({ LOCALADMINUSER: e.detail.value })} />
              </FormField>
              <FormField label="Local admin password">
                <Input type="password" value={editing.LOCALADMINPASS} onChange={(e) => set({ LOCALADMINPASS: e.detail.value })} />
              </FormField>
              <FormField label="Join domain">
                <Input value={editing.JOINDOMAIN} onChange={(e) => set({ JOINDOMAIN: e.detail.value })} placeholder="oopl.dev.mil" />
              </FormField>
              <FormField label="Domain OU">
                <Input value={editing.DOMAINOU} onChange={(e) => set({ DOMAINOU: e.detail.value })} placeholder="OU=Workstations,DC=oopl,DC=dev,DC=mil" />
              </FormField>
              <FormField label="Domain join user">
                <Input value={editing.DOMAINUSER} onChange={(e) => set({ DOMAINUSER: e.detail.value })} placeholder="svc-domainjoin" />
              </FormField>
              <FormField label="Domain join password">
                <Input type="password" value={editing.DOMAINPASS} onChange={(e) => set({ DOMAINPASS: e.detail.value })} />
              </FormField>
            </ColumnLayout>
          </SpaceBetween>
        )}
      </Modal>
    </SpaceBetween>
  );
}
