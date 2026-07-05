import { useState } from "react";
import AppLayout from "@cloudscape-design/components/app-layout";
import ContentLayout from "@cloudscape-design/components/content-layout";
import Header from "@cloudscape-design/components/header";
import Tabs from "@cloudscape-design/components/tabs";
import FilesTab from "./FilesTab";
import LogsTab from "./LogsTab";
import AuditTab from "./AuditTab";

export default function App() {
  const [activeTab, setActiveTab] = useState("files");

  return (
    <AppLayout
      navigationHide
      toolsHide
      content={
        <ContentLayout
          header={
            <Header variant="h1" description="Deploy payloads, deployment logs, and audit trail on the WinDep PV">
              WinDep Deploy — Admin
            </Header>
          }
        >
          <Tabs
            activeTabId={activeTab}
            onChange={(e) => setActiveTab(e.detail.activeTabId)}
            tabs={[
              { id: "files", label: "Files", content: <FilesTab /> },
              { id: "logs", label: "Deployment logs", content: <LogsTab /> },
              { id: "audit", label: "Audit trail", content: <AuditTab /> },
            ]}
          />
        </ContentLayout>
      }
    />
  );
}
