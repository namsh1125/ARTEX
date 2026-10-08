import { Card, CardContent } from "@/components/ui/card";

export function StatTile({ label, value, hint, tone }: { label: string; value: string; hint?: string; tone?: string }) {
  return (
    <Card size="sm" className="gap-1">
      <CardContent>
        <p className="text-muted-foreground text-xs">{label}</p>
        <p className={`text-lg font-semibold ${tone === "red" ? "text-rose-600" : ""}`}>{value}</p>
        {hint && <p className={`text-xs ${tone === "red" ? "text-rose-600" : "text-muted-foreground"}`}>{hint}</p>}
      </CardContent>
    </Card>
  );
}

// formatBacklog는 밀리초 단위 적체 시간을 읽기 쉬운 단위로 표시합니다.
export function formatBacklog(ms: number): string {
  if (!ms) return "—";
  if (ms < 60_000) return `${Math.round(ms / 1000)} 초`;
  if (ms < 3_600_000) return `${Math.round(ms / 60_000)} 분`;
  return `${(ms / 3_600_000).toFixed(1)} 시간`;
}
