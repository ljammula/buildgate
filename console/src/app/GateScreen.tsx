import { Card, CardBody, CardHeader } from "@/ui/Card";

export interface GateScreenProps {
  /** The gate token this browser held was refused: say so, or the operator retries the same link. */
  readonly linkRefused: boolean;
}

/**
 * Shown instead of the app when the server needs a gate token this browser
 * does not hold. Nothing here calls the server: every route would refuse.
 */
export function GateScreen({ linkRefused }: GateScreenProps) {
  return (
    <main className="flex min-h-screen items-center justify-center bg-bg p-6">
      <Card className="w-full max-w-lg">
        <CardHeader>
          <h1 className="text-base font-semibold text-fg">Gate link needed</h1>
        </CardHeader>
        <CardBody className="flex flex-col gap-2 text-sm text-fg">
          <p>
            This console needs its gate link. On the host, run{" "}
            <code className="font-mono text-xs">factoryd gate-token</code> and open the link it
            prints.
          </p>
          {linkRefused ? (
            <p className="text-fg-muted">
              The gate link this browser was using has expired or was replaced.
            </p>
          ) : null}
        </CardBody>
      </Card>
    </main>
  );
}
