import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { ApiError } from "@/domain/apiError";
import { ErrorCallout, describeError } from "@/ui/ErrorDisplay";

describe("describeError classifies known failure modes", () => {
  test("a 401 RunApiException is a generic authorization failure", () => {
    const summary = describeError(new ApiError(401, '{"error":"unauthorized"}'));
    expect(summary.headline).toBe("Not authorized");
    expect(summary.raw).toBe("unauthorized");
  });

  test("a 403 RunApiException is also a generic authorization failure", () => {
    const summary = describeError(new ApiError(403, '{"error":"read endpoint is not authorized"}'));
    expect(summary.headline).toBe("Not authorized");
  });

  test("a 404 RunApiException is reported as not found", () => {
    const summary = describeError(new ApiError(404, '{"error":"no such run"}'));
    expect(summary.headline).toBe("Not found");
    expect(summary.nextStep).toContain("pruned");
  });

  test("another RunApiException status falls back to a generic message", () => {
    const summary = describeError(new ApiError(500, '{"error":"kill switch is unreadable"}'));
    expect(summary.headline).toBe("Request failed (500)");
    expect(summary.raw).toBe("kill switch is unreadable");
  });

  test("a connection-refused failure names factoryd unreachable", () => {
    const summary = describeError(new Error("SocketException: Connection refused"));
    expect(summary.headline).toBe("Can't reach factoryd");
    expect(summary.nextStep).toContain("factoryd serve");
  });

  test("a DNS lookup failure is also reported as unreachable", () => {
    const summary = describeError(new Error("Failed host lookup: model-host"));
    expect(summary.headline).toBe("Can't reach factoryd");
  });

  test("a web fetch failure is also reported as unreachable", () => {
    const summary = describeError(new TypeError("Failed to fetch"));
    expect(summary.headline).toBe("Can't reach factoryd");
  });

  test("an unrecognized error falls back to a generic headline", () => {
    const summary = describeError("Workspace path is required");
    expect(summary.headline).toBe("Something went wrong");
    expect(summary.raw).toBe("Workspace path is required");
  });
});

describe("describeError(startClass: true) (F: serve-start-token)", () => {
  test("a 401/403 points at the console link factoryd serve prints", () => {
    for (const status of [401, 403]) {
      const summary = describeError(
        new ApiError(status, '{"error":"start endpoint is not authorized"}'),
        {
          startClass: true,
        },
      );
      expect(summary.headline).toBe("Not authorized");
      expect(summary.nextStep).toContain("factoryd serve");
      expect(summary.nextStep).toContain("#t=...");
      expect(summary.nextStep).not.toContain("FACTORYD_API_READ_TOKEN");
    }
  });

  test("startClass leaves a non-auth status code unaffected", () => {
    const summary = describeError(new ApiError(500, '{"error":"boom"}'), { startClass: true });
    expect(summary.headline).toBe("Request failed (500)");
  });

  test("startClass leaves a non-RunApiException error unaffected", () => {
    const summary = describeError(new Error("SocketException: Connection refused"), {
      startClass: true,
    });
    expect(summary.headline).toBe("Can't reach factoryd");
  });
});

test("a 403 that states its reason shows the reason, not token advice", () => {
  const summary = describeError(
    new ApiError(
      403,
      '{"error":"workspace is not allowlisted: add it to the session config\'s workspaces list, or submit against a workspace an existing request already uses"}',
    ),
  );
  expect(summary.headline).toBe("Request failed (403)");
  expect(summary.nextStep).toBe(
    "workspace is not allowlisted: add it to the session config's workspaces list, or submit against a workspace an existing request already uses",
  );
});

test("a 403 that is a token refusal keeps the token advice", () => {
  for (const message of [
    "requests endpoint is not authorized",
    "read endpoint is not authorized",
  ]) {
    const summary = describeError(new ApiError(403, JSON.stringify({ error: message })));
    expect(summary.headline).toBe("Not authorized");
    expect(summary.nextStep).toMatch(/token/);
  }
});

describe("ErrorCallout", () => {
  test("shows the server's own message as the next step, one part per line, with nothing left to disclose", () => {
    render(<ErrorCallout error={new ApiError(500, '{"error":"spec.md bad | oracle missing"}')} />);
    expect(screen.getByRole("heading", { name: "Request failed (500)" })).toBeInTheDocument();
    expect(screen.getByText(/spec\.md bad/).textContent).toBe("spec.md bad\noracle missing");
    expect(screen.queryByText("See details below.")).not.toBeInTheDocument();
    expect(screen.queryByText("Details")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
  });

  test("a body that is not the server's error shape stays behind Details", () => {
    render(<ErrorCallout error={new ApiError(502, "<html>bad gateway</html>")} />);
    expect(screen.getByRole("heading", { name: "Request failed (502)" })).toBeInTheDocument();
    expect(screen.getByText("See details below.")).toBeInTheDocument();
    expect(screen.getByText("Details")).toBeInTheDocument();
    expect(screen.getByText("<html>bad gateway</html>")).toBeInTheDocument();
  });

  test("renders untrusted text as text, not markup", () => {
    render(<ErrorCallout error={'<img src=x onerror="alert(1)">'} />);
    expect(document.querySelector("img")).toBeNull();
    expect(screen.getByText('<img src=x onerror="alert(1)">')).toBeInTheDocument();
  });

  test("the Retry button runs the action and is disabled while it is pending", async () => {
    let finish: () => void = () => undefined;
    const onRetry = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          finish = resolve;
        }),
    );
    render(<ErrorCallout error="boom" onRetry={onRetry} />);
    const retry = screen.getByRole("button", { name: "Retry" });
    await userEvent.click(retry);
    expect(onRetry).toHaveBeenCalledTimes(1);
    expect(retry).toBeDisabled();
    finish();
    await vi.waitFor(() => {
      expect(retry).toBeEnabled();
    });
  });
});
