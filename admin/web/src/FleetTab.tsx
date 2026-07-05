import { useCallback, useEffect, useState } from "react";
import Table from "@cloudscape-design/components/table";
import Header from "@cloudscape-design/components/header";
import Button from "@cloudscape-design/components/button";
import SpaceBetween from "@cloudscape-design/components/space-between";
import Box from "@cloudscape-design/components/box";
import Alert from "@cloudscape-design/components/alert";
import Container from "@cloudscape-design/components/container";
import ColumnLayout from "@cloudscape-design/components/column-layout";
import Toggle from "@cloudscape-design/components/toggle";
import StatusIndicator from "@cloudscape-design/components/status-indicator";
import ProgressBar from "@cloudscape-design/components/progress-bar";

interface Machine {
  serial: string;
  state: string;
  percent: number;
  message: string;
  model: string;
  ts: string;
}

const REFRESH_MS = 5000;

function indicator(state: string) {
  switch (state) {
    case "success":
      return <StatusIndicator type="success">success</StatusIndicator>;
    case "failure":
      return <StatusIndicator type="error">failure</StatusIndicator>;
    case "progress":
      return <StatusIndicator type="in-progress">imaging</StatusIndicator>;
    case "start":
      return <StatusIndicator type="pending">starting</StatusIndicator>;
    default:
      return <StatusIndicator type="info">{state || "—"}</StatusIndicator>;
  }
}

function Metric({ label, value }: { label: string; value: number }) {
  return (
    <div>
      <Box variant="awsui-key-label">{label}</Box>
      <Box fontSize="display-l" fontWeight="bold">
        {value}
      </Box>
    </div>
  );
}

export default function FleetTab() {
  const [items, setItems] = useState<Machine[]>([]);
  const [loading, setLoading] = useState(false);
  const [auto, setAuto] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [updated, setUpdated] = useState<string>("");

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const r = await fetch(`/api/fleet`);
      if (!r.ok) throw new Error(`fleet failed (${r.status})`);
      setItems(await r.json());
      setUpdated(new Date().toLocaleTimeString());
      setError(null);
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
    if (!auto) return;
    const id = setInterval(() => void refresh(), REFRESH_MS);
    return () => clearInterval(id);
  }, [auto, refresh]);

  const counts = {
    total: items.length,
    imaging: items.filter((m) => m.state === "progress" || m.state === "start").length,
    success: items.filter((m) => m.state === "success").length,
    failure: items.filter((m) => m.state === "failure").length,
  };

  return (
    <SpaceBetween size="l">
      {error && (
        <Alert type="error" dismissible onDismiss={() => setError(null)}>
          {error}
        </Alert>
      )}

      <Container
        header={
          <Header
            variant="h2"
            description={updated ? `Last updated ${updated}` : undefined}
            actions={
              <SpaceBetween direction="horizontal" size="s">
                <Toggle checked={auto} onChange={(e) => setAuto(e.detail.checked)}>
                  Auto-refresh (5s)
                </Toggle>
                <Button iconName="refresh" loading={loading} onClick={() => void refresh()}>
                  Refresh
                </Button>
              </SpaceBetween>
            }
          >
            Fleet status
          </Header>
        }
      >
        <ColumnLayout columns={4} variant="text-grid">
          <Metric label="Machines" value={counts.total} />
          <Metric label="Imaging" value={counts.imaging} />
          <Metric label="Succeeded" value={counts.success} />
          <Metric label="Failed" value={counts.failure} />
        </ColumnLayout>
      </Container>

      <Table<Machine>
        items={items}
        loading={loading && items.length === 0}
        loadingText="Loading fleet"
        variant="container"
        trackBy="serial"
        header={<Header variant="h2" counter={`(${items.length})`}>Machines</Header>}
        columnDefinitions={[
          { id: "serial", header: "Serial", cell: (m) => m.serial, isRowHeader: true },
          { id: "state", header: "State", cell: (m) => indicator(m.state) },
          {
            id: "progress",
            header: "Progress",
            cell: (m) =>
              m.state === "progress" || m.state === "start" ? (
                <ProgressBar value={m.percent} />
              ) : (
                `${m.percent}%`
              ),
          },
          { id: "message", header: "Message", cell: (m) => m.message || "" },
          { id: "model", header: "Model", cell: (m) => m.model || "" },
          { id: "ts", header: "Last seen", cell: (m) => m.ts.replace("T", " ").replace("Z", ""), width: 170 },
        ]}
        empty={
          <Box textAlign="center" color="inherit">
            <b>No machines reporting</b>
            <Box variant="p" color="inherit">
              Status appears here as machines boot into WinPE and start imaging.
            </Box>
          </Box>
        }
      />
    </SpaceBetween>
  );
}
