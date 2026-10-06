import { type ReactNode, useId } from "react";

import { Card, CardBody, CardHeader, CardTitle } from "@/ui/Card";
import { cn } from "@/ui/cn";

export interface PanelProps {
  readonly title: string;
  /** Controls on the right of the title row. */
  readonly actions?: ReactNode;
  readonly children: ReactNode;
  readonly className?: string;
  readonly testId?: string;
}

/** A titled card; a region named by its title, so a test and a screen reader can find it. */
export function Panel({ title, actions, children, className, testId }: PanelProps) {
  const titleId = useId();
  return (
    <Card
      role="region"
      aria-labelledby={titleId}
      className={cn("min-w-0", className)}
      {...(testId === undefined ? {} : { "data-testid": testId })}
    >
      <CardHeader className="py-2.5">
        <CardTitle id={titleId} className="text-sm">
          {title}
        </CardTitle>
        {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
      </CardHeader>
      <CardBody className="flex flex-col gap-3">{children}</CardBody>
    </Card>
  );
}
