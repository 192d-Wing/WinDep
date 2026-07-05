import { useCallback, useEffect, useState } from "react";
import Table from "@cloudscape-design/components/table";
import Header from "@cloudscape-design/components/header";
import Button from "@cloudscape-design/components/button";
import SpaceBetween from "@cloudscape-design/components/space-between";
import Box from "@cloudscape-design/components/box";
import Badge from "@cloudscape-design/components/badge";
import Alert from "@cloudscape-design/components/alert";
import { humanSize } from "./util";

interface AuditEntry {
  ts: string;
  action: string;
  category: string;
  path: string;
  source: string;
  size: number;
  status: number;
}

const ACTION_COLOR: Record<string, "blue" | "green" | "red" | "grey"> = {
  upload: "green",
  mkdir: "blue",
  delete: "red",
  download: "grey",
  list: "grey",
};

export default function AuditTab() {
  const [items, setItems] = useState<AuditEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const r = await fetch(`/api/audit?limit=1000`);
      if (!r.ok) throw new Error(`audit failed (${r.status})`);
      setItems(await r.json());
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, []);

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
      <Table<AuditEntry>
        items={items}
        loading={loading}
        loadingText="Loading audit trail"
        variant="container"
        header={
          <Header
            variant="h2"
            counter={`(${items.length})`}
            actions={
              <Button iconName="refresh" loading={loading} onClick={() => void refresh()}>
                Refresh
              </Button>
            }
          >
            Audit trail
          </Header>
        }
        columnDefinitions={[
          { id: "ts", header: "Time", cell: (e) => e.ts.replace("T", " ").replace("Z", ""), width: 170 },
          { id: "action", header: "Action", cell: (e) => <Badge color={ACTION_COLOR[e.action] ?? "grey"}>{e.action}</Badge> },
          { id: "target", header: "Target", cell: (e) => [e.category, e.path].filter(Boolean).join("/") || "—" },
          { id: "source", header: "Source", cell: (e) => e.source || "—" },
          { id: "size", header: "Size", cell: (e) => (e.size > 0 ? humanSize(e.size) : "") },
          { id: "status", header: "Status", cell: (e) => e.status },
        ]}
        empty={
          <Box textAlign="center" color="inherit">
            <b>No audit entries</b>
          </Box>
        }
      />
    </SpaceBetween>
  );
}
