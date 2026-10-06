import { Card, CardBody } from "@/ui/Card";

export interface EvidenceCardProps {
  readonly title: string;
  readonly lines: readonly string[];
}

/** One recorded item (a gate, a compose phase, a notification, an override) as a title and its lines. */
export function EvidenceCard({ title, lines }: EvidenceCardProps) {
  return (
    <Card className="bg-surface-sunken">
      <CardBody className="flex flex-col gap-0.5 p-3">
        <h3 className="text-sm font-semibold text-fg">{title}</h3>
        {lines.map((line, i) => (
          <p key={i} className="text-xs break-words text-fg-muted">
            {line}
          </p>
        ))}
      </CardBody>
    </Card>
  );
}
