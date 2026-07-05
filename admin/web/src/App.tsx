import { useEffect, useState } from "react";
import { applyMode, Mode } from "@cloudscape-design/global-styles";
import AppLayout from "@cloudscape-design/components/app-layout";
import ContentLayout from "@cloudscape-design/components/content-layout";
import Header from "@cloudscape-design/components/header";
import Tabs from "@cloudscape-design/components/tabs";
import Toggle from "@cloudscape-design/components/toggle";
import FilesTab from "./FilesTab";
import MachinesTab from "./MachinesTab";
import LogsTab from "./LogsTab";
import AuditTab from "./AuditTab";

const MODE_KEY = "windep-admin-mode";

function prefersDark(): boolean {
  const saved = localStorage.getItem(MODE_KEY);
  if (saved) return saved === "dark";
  return globalThis.matchMedia?.("(prefers-color-scheme: dark)").matches ?? false;
}

// Apply the saved/OS mode before first paint so there's no light-to-dark flash.
applyMode(prefersDark() ? Mode.Dark : Mode.Light);

export default function App() {
  const [activeTab, setActiveTab] = useState("files");
  const [dark, setDark] = useState(prefersDark);

  useEffect(() => {
    applyMode(dark ? Mode.Dark : Mode.Light);
    localStorage.setItem(MODE_KEY, dark ? "dark" : "light");
  }, [dark]);

  return (
    <AppLayout
      navigationHide
      toolsHide
      content={
        <ContentLayout
          header={
            <Header
              variant="h1"
              description="Deploy payloads, deployment logs, and audit trail on the WinDep PV"
              actions={
                <Toggle checked={dark} onChange={(e) => setDark(e.detail.checked)}>
                  Dark mode
                </Toggle>
              }
            >
              WinDep Deploy — Admin
            </Header>
          }
        >
          <Tabs
            activeTabId={activeTab}
            onChange={(e) => setActiveTab(e.detail.activeTabId)}
            tabs={[
              { id: "files", label: "Files", content: <FilesTab /> },
              { id: "machines", label: "Machines", content: <MachinesTab /> },
              { id: "logs", label: "Deployment logs", content: <LogsTab /> },
              { id: "audit", label: "Audit trail", content: <AuditTab /> },
            ]}
          />
        </ContentLayout>
      }
    />
  );
}
