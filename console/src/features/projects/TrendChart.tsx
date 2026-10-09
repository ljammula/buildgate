import { type ProjectTrend, bucketLabel, trendChartSummary, trendPointText } from "@/domain/trend";

export interface TrendChartProps {
  readonly trend: ProjectTrend;
}

// The drawing's own units; the SVG scales to the width it is given.
const W = 640;
const H = 150;
const LEFT = 34;
const RIGHT = 14;
const TOP = 10;
const BOTTOM = 22;
const GRID = [0, 50, 100];

function yOf(percent: number): number {
  return TOP + ((100 - percent) / 100) * (H - TOP - BOTTOM);
}

/** One period is drawn in the middle; several are spread edge to edge. */
function xOf(index: number, count: number): number {
  const span = W - LEFT - RIGHT;
  return count === 1 ? LEFT + span / 2 : LEFT + (index / (count - 1)) * span;
}

/**
 * The one-shot acceptance rate of each period as a line on one 0-100% axis,
 * above the table that holds every value. A period with no ticket has no
 * rate: it is a break in the line, never a point at 0%. The tickets behind a
 * point are in its hover text and in the table, not on a second axis.
 *
 * It is one image to a screen reader, named by a sentence that says what the
 * line shows. Nothing is drawn when no period has a ticket.
 */
export function TrendChart({ trend }: TrendChartProps) {
  const summary = trendChartSummary(trend);
  if (summary === "") return null;
  const count = trend.buckets.length;
  const points = trend.buckets.map((bucket, i) => ({
    bucket,
    x: xOf(i, count),
    y: bucket.metrics.oneShotRate === null ? null : yOf(bucket.metrics.oneShotRate * 100),
  }));
  // "M" after a gap starts a new stroke, so the line never crosses a period with no ticket.
  let path = "";
  let pen = false;
  for (const p of points) {
    if (p.y !== null) path += `${pen ? "L" : "M"}${p.x.toFixed(1)} ${p.y.toFixed(1)}`;
    pen = p.y !== null;
  }
  const first = points[0];
  const last = points.at(-1);
  return (
    <figure data-testid="trend-chart" className="flex max-w-2xl flex-col gap-1">
      <figcaption className="text-xs text-fg-muted">
        One-shot rate per {trend.bucketDays === 7 ? "week" : `${trend.bucketDays} days`}
      </figcaption>
      <svg role="img" aria-label={summary} viewBox={`0 0 ${W} ${H}`} className="w-full">
        {GRID.map((percent) => (
          <g key={percent}>
            <line
              x1={LEFT}
              x2={W - RIGHT}
              y1={yOf(percent)}
              y2={yOf(percent)}
              className="stroke-border"
              strokeWidth={1}
            />
            <text
              x={LEFT - 6}
              y={yOf(percent) + 3}
              textAnchor="end"
              className="fill-fg-subtle text-[10px]"
            >
              {percent}%
            </text>
          </g>
        ))}
        {points.map((p) => (
          <line
            key={p.bucket.start}
            x1={p.x}
            x2={p.x}
            y1={yOf(0)}
            y2={yOf(0) + 4}
            className="stroke-border-strong"
            strokeWidth={1}
          />
        ))}
        {first === undefined ? null : (
          <text
            x={first.x}
            y={H - 4}
            textAnchor={count === 1 ? "middle" : "start"}
            className="fill-fg-subtle font-mono text-[10px]"
          >
            {bucketLabel(first.bucket)}
          </text>
        )}
        {last === undefined || count === 1 ? null : (
          <text
            x={last.x}
            y={H - 4}
            textAnchor="end"
            className="fill-fg-subtle font-mono text-[10px]"
          >
            {bucketLabel(last.bucket)}
          </text>
        )}
        <path
          d={path}
          fill="none"
          strokeWidth={2}
          strokeLinejoin="round"
          className="stroke-accent"
        />
        {points.map((p) =>
          p.y === null ? null : (
            <g key={p.bucket.start} data-testid="trend-chart-point">
              <circle
                cx={p.x}
                cy={p.y}
                r={4}
                strokeWidth={2}
                className="fill-accent stroke-surface"
              />
              {/* A hit area larger than the mark, so the value is easy to hover. */}
              <circle cx={p.x} cy={p.y} r={12} fill="transparent">
                <title>{trendPointText(p.bucket, trend.bucketDays)}</title>
              </circle>
            </g>
          ),
        )}
      </svg>
    </figure>
  );
}
