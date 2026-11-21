import type { ClusterReport } from "../domain";
import { OrkaPod } from "../components/OrkaPod";

export function Agents({ report }: { report: ClusterReport }) {
  return <div className="page-content"><div className="page-intro"><span className="section-label">Real cluster data</span><p>The aquarium maps discovered Agents and active Tasks from <code>{report.orka.namespace}</code>. Cast the status rod to inspect one.</p></div><OrkaPod agents={report.agents} tasks={report.tasks} /></div>;
}
