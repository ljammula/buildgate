import { type SyntheticEvent, useState } from "react";

import type { TokenOffer } from "@/app/session";
import { Button } from "@/ui/Button";
import { Card, CardBody, CardHeader } from "@/ui/Card";
import { Field, Input } from "@/ui/Input";
import { TextWithCode } from "@/ui/TextWithCode";

export interface GateScreenProps {
  /** The gate token this browser held was refused: say so, or the operator pastes the same one. */
  readonly tokenRefused: boolean;
  /** Offers the pasted token to the server (ConsoleSession.offerToken). */
  readonly offerToken: (token: string) => Promise<TokenOffer>;
  /** Called once a pasted token was accepted and stored: start the console again with it. */
  readonly onAccepted: () => void;
}

const problems: Record<Exclude<TokenOffer, "accepted">, string> = {
  refused:
    "That token was not accepted. It may have expired or been replaced: run `factoryd gate-token` on the host again.",
  "no-answer":
    "The server did not answer. The token was not checked: try again, or reload the page.",
  "not-kept":
    "The server accepted the token, but this browser will not keep it for the tab (its storage is blocked). Allow site data for this address and try again.",
};

/**
 * Shown instead of the app when the server needs a gate token this browser
 * does not hold. The operator pastes the token here, so it never has to be
 * part of a URL. The field is a text field masked with CSS, not a password
 * field: a browser offers to save what a password field held, and a saved
 * token would outlive the tab. Nothing here calls the server until the token
 * is offered: every other route would refuse.
 */
export function GateScreen({ tokenRefused, offerToken, onAccepted }: GateScreenProps) {
  const [token, setToken] = useState("");
  const [checking, setChecking] = useState(false);
  const [problem, setProblem] = useState<Exclude<TokenOffer, "accepted"> | null>(null);

  const submit = (event: SyntheticEvent) => {
    event.preventDefault();
    if (checking || token.trim() === "") return;
    setChecking(true);
    setProblem(null);
    void offerToken(token).then((offer) => {
      setChecking(false);
      if (offer === "accepted") {
        onAccepted();
        return;
      }
      // A refused token is cleared; one that was never checked is kept.
      if (offer === "refused") setToken("");
      setProblem(offer);
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
            <TextWithCode text="`factoryd gate-token`" /> and paste the token it prints here.
          </p>
          {tokenRefused ? (
            <p className="text-fg-muted">
              The gate token this browser was using has expired or was replaced.
            </p>
          ) : null}
          <form className="flex items-end gap-2" onSubmit={submit}>
            <Field
              label="Gate token"
              className="grow"
              error={
                problem ? (
                  <span role="alert">
                    <TextWithCode text={problems[problem]} />
                  </span>
                ) : undefined
              }
            >
              <Input
                type="text"
                name="gate-token"
                autoComplete="off"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                autoFocus
                className="[-webkit-text-security:disc]"
                value={token}
                onChange={(event) => {
                  setToken(event.target.value);
                }}
              />
            </Field>
            <Button type="submit" variant="primary" disabled={checking || token.trim() === ""}>
              {checking ? "Checking" : "Open console"}
            </Button>
          </form>
        </CardBody>
      </Card>
    </main>
  );
}
