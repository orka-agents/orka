import { Cloud, Play, RefreshCw, Rocket } from "lucide-react";
import type { ClusterReport, PageId } from "../domain";
import { relativeTime } from "../lib/format";

const pageCopy: Record<PageId, { kicker: string; title: string }> = {
  discover: { kicker: "Read-only discovery", title: "Cluster crawl" },
  tasks: { kicker: "Orka execution", title: "Run work" },
  agents: { kicker: "Live habitat", title: "Agent pod" },
  install: { kicker: "Guarded bootstrap", title: "Quick deploy" },
};

export function Header({
  page, report, refreshing, onRefresh, onPrimary,
}: {
  page: PageId;
  report?: ClusterReport;
  refreshing: boolean;
  onRefresh: () => void;
  onPrimary: () => void;
}) {
  const copy = pageCopy[page];
  const ready = report?.orka.state === "ready";
  return (
    <header className="topbar">
      <div><span className="page-kicker">{copy.kicker}</span><h1>{copy.title}</h1></div>
      <div className="topbar-actions">
        {report && <span className={`cluster-state state-${report.orka.state}`}>{report.orka.state}</span>}
        <span className="sync-label"><Cloud size={15} />{report ? relativeTime(report.crawledAt) : "Not crawled"}</span>
        <button className="icon-button" type="button" onClick={onRefresh} aria-label="Crawl cluster again" disabled={refreshing}>
          <RefreshCw className={refreshing ? "spin" : ""} size={18} />
        </button>
        <button className="button button-primary primary-action" type="button" onClick={onPrimary}>
          {ready ? <Play size={17} /> : <Rocket size={17} />}{ready ? "Run task" : "Quick deploy"}
        </button>
      </div>
    </header>
  );
}
