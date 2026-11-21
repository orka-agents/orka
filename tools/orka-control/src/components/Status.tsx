export function StatusPill({ value }: { value: string }) {
  const normalized = value.toLowerCase();
  const tone = ["ready", "running", "succeeded", "bound", "healthy"].includes(normalized)
    ? "positive"
    : ["pending", "scheduled", "finalizing", "installing", "partial"].includes(normalized)
      ? "progress"
      : ["absent", "idle", "unknown"].includes(normalized) ? "neutral" : "negative";
  return <span className={`status-pill status-${tone}`}><span className="status-dot" />{value}</span>;
}
