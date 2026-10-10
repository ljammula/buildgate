import { type SyntheticEvent, useId, useState } from "react";

import { Button } from "@/ui/Button";
import { Card, CardBody, CardHeader } from "@/ui/Card";
import { Input } from "@/ui/Input";

export interface GateScreenProps {
  /** The gate token this browser held was refused: say so, or the operator pastes the same one. */
  readonly tokenRefused: boolean;
  /** Offers the pasted token to the server; true once it is accepted and stored. */
  readonly offerToken: (token: string) => Promise<boolean>;
  /** Called once a pasted token was accepted: start the console again with it. */
  readonly onAccepted: () => void;
}

/**
 * Shown instead of the app when the server needs a gate token this browser
 * does not hold. The operator pastes the token here, so it never has to be
 * part of a URL. The field is a password field: the token is not shown, and
 * the browser is told not to offer to remember it. Nothing here calls the
 * server until the token is offered: every other route would refuse.
 */
export function GateScreen({ tokenRefused, offerToken, onAccepted }: GateScreenProps) {
  const fieldId = useId();
  const [token, setToken] = useState("");
  const [checking, setChecking] = useState(false);
  const [refused, setRefused] = useState(false);

  const submit = (event: SyntheticEvent) => {
    event.preventDefault();
    if (checking || token.trim() === "") return;
    setChecking(true);
    setRefused(false);
    void offerToken(token).then((accepted) => {
      setChecking(false);
      if (accepted) {
        onAccepted();
        return;
      }
      setToken("");
      setRefused(true);
    });
  };

  return (
    <main className="flex min-h-screen items-center justify-center bg-bg p-6">
      <Card className="w-full max-w-lg">
        <CardHeader>
          <h1 className="text-base font-semibold text-fg">Gate token needed</h1>
        </CardHeader>
        <CardBody className="flex flex-col gap-3 text-sm text-fg">
          <p>
            This console needs its gate token. On the host, run{" "}
            <code className="font-mono text-xs">factoryd gate-token</code> and paste the token it
            prints here.
          </p>
          {tokenRefused ? (
            <p className="text-fg-muted">
              The gate token this browser was using has expired or was replaced.
            </p>
          ) : null}
          <form className="flex flex-col gap-2" onSubmit={submit}>
            <label className="text-xs font-medium text-fg-muted" htmlFor={fieldId}>
              Gate token
            </label>
            <div className="flex gap-2">
              <Input
                id={fieldId}
                type="password"
                autoComplete="off"
                spellCheck={false}
                value={token}
                aria-invalid={refused}
                onChange={(event) => {
                  setToken(event.target.value);
                }}
              />
              <Button type="submit" variant="primary" disabled={checking || token.trim() === ""}>
                {checking ? "Checking" : "Open console"}
              </Button>
            </div>
            {refused ? (
              <p role="alert" className="text-fg-muted">
                That token was not accepted. It may have expired or been replaced: run{" "}
                <code className="font-mono text-xs">factoryd gate-token</code> again.
              </p>
            ) : null}
          </form>
        </CardBody>
      </Card>
    </main>
  );
}
