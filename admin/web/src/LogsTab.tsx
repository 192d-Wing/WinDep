import { useCallback, useEffect, useState } from "react";
import Table from "@cloudscape-design/components/table";
import Header from "@cloudscape-design/components/header";
import Button from "@cloudscape-design/components/button";
import SpaceBetween from "@cloudscape-design/components/space-between";
import Input from "@cloudscape-design/components/input";
import Box from "@cloudscape-design/components/box";
import StatusIndicator from "@cloudscape-design/components/status-indicator";
import Alert from "@cloudscape-design/components/alert";

interface DeployEvent {
  ts: string;
  kind: string;
  serial: string;
  mac: string;
  state: string;
  percent: number;
  message: string;
  model: string;
  level: string;
}

function stateIndicator(e: DeployEvent) {
  if (e.kind !== "status") return e.level || "—";
  const map: Record<string, "success" | "in-progress" | "error" | "info"> = {
    success: "success",
    progress: "in-progress",
    failure: "error",
    start: "info",
  };
  const type = map[e.state] ?? "info";
  return <StatusIndicator type={type}>{e.state || "—"}</StatusIndicator>;
}

export default function LogsTab() {
  const [items, setItems] = useState<DeployEvent[]>([]);
  const [serial, setSerial] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const q = new URLSearchParams({ limit: "500" });
      if (serial.trim()) q.set("serial", serial.trim());
      const r = await fetch(`/api/logs?${q}`);
      if (!r.ok) throw new Error(`logs failed (${r.status})`);
      setItems(await r.json());
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, [serial]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  return (
    <SpaceBetween size="l">
      {error && (
        <Alert type="error" dismissible onDismiss={() => setError(null)}>
          {error}
        </Alert>
      )}
      <Table<DeployEvent>
        items={items}
        loading={loading}
        loadingText="Loading deployment logs"
        variant="container"
        header={
          <Header
            variant="h2"
            counter={`(${items.length})`}
            actions={
              <SpaceBetween direction="horizontal" size="xs">
                <Input
                  type="search"
                  value={serial}
                  onChange={(e) => setSerial(e.detail.value)}
                  placeholder="Filter by serial"
                />
                <Button iconName="refresh" loading={loading} onClick={() => void refresh()}>
                  Refresh
                </Button>
              </SpaceBetween>
            }
          >
            Deployment logs
          </Header>
        }
        columnDefinitions={[
          { id: "ts", header: "Time", cell: (e) => e.ts.replace("T", " ").replace("Z", ""), width: 170 },
          { id: "serial", header: "Serial", cell: (e) => e.serial || "—" },
          { id: "state", header: "State / Level", cell: stateIndicator },
          { id: "percent", header: "%", cell: (e) => (e.kind === "status" ? `${e.percent}%` : ""), width: 70 },
          { id: "message", header: "Message", cell: (e) => e.message },
          { id: "model", header: "Model", cell: (e) => e.model || "" },
        ]}
        empty={
          <Box textAlign="center" color="inherit">
            <b>No deployment events yet</b>
            <Box variant="p" color="inherit">
              WinPE status and logs appear here as machines are imaged.
            </Box>
          </Box>
        }
      />
    </SpaceBetween>
  );
}
