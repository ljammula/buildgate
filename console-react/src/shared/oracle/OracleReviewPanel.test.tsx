import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { sha256Hex, sha256HexBytes } from "@/domain/contentHash";
import { OracleDraftingSection } from "@/shared/oracle/OracleDraftingSection";
import { OracleReviewPanel } from "@/shared/oracle/OracleReviewPanel";
import { enc, oracleServer, requestSummary, tileOf, toggle } from "@/shared/oracle/oracleTestKit";
import { renderApp } from "@/test/render";

const MANIFEST =
  '[{"criterion":"1. Returns 200.","oracle_file":"a_oracle_test.go",' +
  '"criterion_index":1,"target_path":"internal/a/a.go","supersedes":[]},' +
  '{"criterion":"2. Code is clean.","oracle_file":null,' +
  '"rationale":"judgment call","criterion_index":2,"supersedes":[]}]';

const FILES = {
  "MANIFEST.json": MANIFEST,
  "RUN_COMMAND.txt": "go test ./...\n",
  "a_oracle_test.go": "package a\n",
};

type Server = ReturnType<typeof oracleServer>;

function setup(
  server: Server,
  options: {
    writesEnabled?: boolean;
    onApprove?: (expected: Record<string, string>) => Promise<void>;
    request?: ReturnType<typeof requestSummary>;
  } = {},
) {
  const onApprove = vi.fn(options.onApprove ?? (async () => {}));
  const view = renderApp(
    <OracleReviewPanel
      request={options.request ?? requestSummary()}
      canAct
      onApprove={onApprove}
    />,
    { server: server.server, config: { writesEnabled: options.writesEnabled ?? true } },
  );
  return { ...view, onApprove };
}

const approveButton = () => screen.getByRole("button", { name: /^Approve/ });
const panel = () => screen.getByTestId("oracle-review-panel");
const contentOf = (keyId: string) =>
  within(screen.getByTestId(`oracle-content-${keyId}`)).getByRole("region").textContent;

async function loaded() {
  await screen.findByRole("button", { name: "Reload files" });
}

/** Waits for a background reload to finish (Approve is disabled while one runs). */
async function settled() {
  await waitFor(() => {
    expect(screen.getByRole("button", { name: "Reload files" })).toBeEnabled();
  });
}

test("lists files with sha256, keeps Approve disabled until every file has been opened, then approves with the displayed hashes", async () => {
  const server = oracleServer(FILES);
  const { onApprove } = setup(server);
  await loaded();

  expect(screen.getByText("RUN_COMMAND.txt")).toBeInTheDocument();
  expect(
    screen.getByText(new RegExp(sha256Hex("go test ./...\n").slice(0, 12))),
  ).toBeInTheDocument();
  expect(approveButton()).toBeDisabled();
  expect(
    screen.getByText("Files (0 of 3 shown) -- open every one to enable Approve"),
  ).toBeInTheDocument();
  expect(screen.getByText("Open 3 more files to approve.")).toBeInTheDocument();

  await toggle(panel(), "RUN_COMMAND.txt");
  await screen.findByTestId("oracle-content-RUN_COMMAND.txt");
  const pre = within(screen.getByTestId("oracle-content-RUN_COMMAND.txt")).getByRole("region");
  expect(pre.textContent).toBe("go test ./...\n");
  expect(pre).toHaveClass("font-mono");
  expect(approveButton()).toBeDisabled();

  await toggle(panel(), "a_oracle_test.go");
  await screen.findByText("Open 1 more file to approve.");
  expect(approveButton()).toBeDisabled();
  await toggle(panel(), "MANIFEST.json");
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });

  await userEvent.click(approveButton());
  expect(onApprove).toHaveBeenCalledWith({
    "oracle/MANIFEST.json": sha256Hex(MANIFEST),
    "oracle/RUN_COMMAND.txt": sha256Hex("go test ./...\n"),
    "oracle/a_oracle_test.go": sha256Hex("package a\n"),
  });
});

test("renders MANIFEST criteria coverage, including uncovered criteria and their rationale", async () => {
  setup(oracleServer(FILES));
  const coverage = await screen.findByTestId("oracle-coverage");
  expect(within(coverage).getByText(/Covered by a_oracle_test.go/)).toBeInTheDocument();
  expect(within(coverage).getByText(/Not covered by an oracle: judgment call/)).toBeInTheDocument();
  // Fetching MANIFEST for coverage does not count as showing its file.
  expect(approveButton()).toBeDisabled();
});

test("shows the problems list and keeps Approve disabled even with every file opened", async () => {
  const server = oracleServer({ "a_oracle_test.go": "package a\n" });
  server.problems = ["no RUN_COMMAND.txt -- add it before approving"];
  setup(server);
  await loaded();

  expect(screen.getByText(/no RUN_COMMAND.txt -- add it before approving/)).toBeInTheDocument();
  await toggle(panel(), "a_oracle_test.go");
  await screen.findByTestId("oracle-content-a_oracle_test.go");
  expect(approveButton()).toBeDisabled();
});

test("an empty oracle/ approves as a skip with no hashes", async () => {
  const { onApprove } = setup(oracleServer({}));
  await loaded();

  expect(screen.getByTestId("oracle-empty")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Approve (skip oracle)" })).toBeEnabled();
  await userEvent.click(approveButton());
  expect(onApprove).toHaveBeenCalledWith({});
});

test("a file that changed on the server after it was shown must be re-shown before approval, and the new hash is what is sent", async () => {
  const server = oracleServer({ "RUN_COMMAND.txt": "go test ./...\n" });
  const { onApprove } = setup(server);
  await loaded();
  await toggle(panel(), "RUN_COMMAND.txt");
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });

  server.files.set("RUN_COMMAND.txt", enc("go test ./internal/...\n"));
  await userEvent.click(screen.getByRole("button", { name: "Reload files" }));

  // The open tile re-fetched the changed bytes, so it shows the new content
  // and approval carries the new hash.
  await waitFor(() => {
    expect(contentOf("RUN_COMMAND.txt")).toBe("go test ./internal/...\n");
  });
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });
  await userEvent.click(approveButton());
  expect(onApprove).toHaveBeenCalledWith({
    "oracle/RUN_COMMAND.txt": sha256Hex("go test ./internal/...\n"),
  });
});

test("editing RUN_COMMAND.txt PUTs the content and shows the saved file", async () => {
  const server = oracleServer(FILES);
  const { server: fake } = setup(server);
  await loaded();
  await toggle(panel(), "RUN_COMMAND.txt");

  await userEvent.click(await screen.findByRole("button", { name: "Edit" }));
  server.files.set("RUN_COMMAND.txt", enc("go test ./internal/a/...\n"));
  fireEvent.change(screen.getByRole("textbox", { name: "RUN_COMMAND.txt content" }), {
    target: { value: "go test ./internal/a/...\n" },
  });
  await userEvent.click(screen.getByRole("button", { name: "Save" }));

  await waitFor(() => {
    expect(contentOf("RUN_COMMAND.txt")).toBe("go test ./internal/a/...\n");
  });
  expect(fake.sent("PUT /requests/req-1/oracle/RUN_COMMAND.txt")[0]?.body).toEqual({
    content: "go test ./internal/a/...\n",
  });
  expect(screen.queryByRole("textbox", { name: "RUN_COMMAND.txt content" })).toBeNull();
});

test("a 422 from saving RUN_COMMAND.txt renders inline and keeps the editor open", async () => {
  const server = oracleServer(FILES);
  server.putError = "RUN_COMMAND.txt does not run a test";
  setup(server);
  await loaded();
  await toggle(panel(), "RUN_COMMAND.txt");
  await userEvent.click(await screen.findByRole("button", { name: "Edit" }));
  await userEvent.click(screen.getByRole("button", { name: "Save" }));

  expect(
    await screen.findByText("Could not save: RUN_COMMAND.txt does not run a test"),
  ).toBeInTheDocument();
  expect(screen.getByRole("textbox", { name: "RUN_COMMAND.txt content" })).toBeInTheDocument();
});

test("a file collapsed and then rewritten on the server is not shown again until re-opened: Approve stays disabled after Reload", async () => {
  const server = oracleServer({ "RUN_COMMAND.txt": "go test ./...\n" });
  const { onApprove } = setup(server);
  await loaded();
  await toggle(panel(), "RUN_COMMAND.txt");
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });

  await toggle(panel(), "RUN_COMMAND.txt"); // collapse
  expect(approveButton()).toBeDisabled();

  server.files.set("RUN_COMMAND.txt", enc("rm -rf /\n"));
  await userEvent.click(screen.getByRole("button", { name: "Reload files" }));
  await settled();
  expect(approveButton()).toBeDisabled();
  // A collapsed changed file must not be auto-refetched.
  expect(server.server.sent("GET /requests/req-1/oracle/RUN_COMMAND.txt")).toHaveLength(1);

  await toggle(panel(), "RUN_COMMAND.txt");
  await waitFor(() => {
    expect(contentOf("RUN_COMMAND.txt")).toBe("rm -rf /\n");
  });
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });
  await userEvent.click(approveButton());
  expect(onApprove).toHaveBeenCalledWith({ "oracle/RUN_COMMAND.txt": sha256Hex("rm -rf /\n") });
});

test("invisible and bidi characters render as visible escapes, empty files get a marker, and the editor previews escapes", async () => {
  const server = oracleServer({
    "RUN_COMMAND.txt": "go test‮ ./...​\x01\n",
    "empty_test.go": "",
  });
  setup(server);
  await loaded();
  await toggle(panel(), "RUN_COMMAND.txt");
  await waitFor(() => {
    expect(contentOf("RUN_COMMAND.txt")).toBe("go test\\u{202E} ./...\\u{200B}\\u{1}\n");
  });
  await toggle(panel(), "empty_test.go");
  expect(await screen.findByTestId("oracle-empty-file")).toBeInTheDocument();
  expect(screen.getByText("(empty file)")).toBeInTheDocument();

  await userEvent.click(screen.getByRole("button", { name: "Edit" }));
  expect(screen.getByTestId("oracle-run-command-preview")).toBeInTheDocument();
  fireEvent.change(screen.getByRole("textbox", { name: "RUN_COMMAND.txt content" }), {
    target: { value: "go test ./..." },
  });
  expect(screen.queryByTestId("oracle-run-command-preview")).toBeNull();
});

test("a refused approval shows the server message without the JSON envelope and re-lists the files", async () => {
  // The panel's half: the parent reports the refusal; the panel re-lists.
  const server = oracleServer({ "RUN_COMMAND.txt": "go test ./...\n" });
  const { onApprove } = setup(server, {
    onApprove: () =>
      Promise.reject(new Error("oracle/RUN_COMMAND.txt artifact changed since it was fetched")),
  });
  await loaded();
  await toggle(panel(), "RUN_COMMAND.txt");
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });
  const before = server.server.sent("GET /requests/req-1/oracle").length;
  await userEvent.click(approveButton());
  await waitFor(() => {
    expect(server.server.sent("GET /requests/req-1/oracle").length).toBeGreaterThan(before);
  });
  expect(onApprove).toHaveBeenCalledTimes(1);
});

test("every manifest-derived, problem, proposed-command and file name string shows hidden characters as visible escapes", async () => {
  const manifest = JSON.stringify([
    {
      criterion: "1. Cri‮terion",
      oracle_file: "a​_test.go",
      target_path: "x/⁦y.go",
      supersedes: ["old؜.go"],
      criterion_index: 1,
    },
    { criterion: "2. Other", oracle_file: null, rationale: "why‎", criterion_index: 2 },
  ]);
  const server = oracleServer({ "MANIFEST.json": manifest, "n‮ame_test.go": "x" });
  server.problems = ["bad‮file"];
  server.draft = {
    status: "failed",
    detail: "det​ail",
    proposed_command: "go test‮ ./...",
  };
  setup(server);
  const coverage = await screen.findByTestId("oracle-coverage");

  const text = coverage.textContent;
  expect(text).toContain("Cri\\u{202E}terion");
  expect(text).toContain("a\\u{200B}_test.go");
  expect(text).toContain("x/\\u{2066}y.go");
  expect(text).toContain("old\\u{61C}.go");
  expect(text).toContain("why\\u{200E}");
  expect(screen.getByTestId("oracle-problems").textContent).toContain("bad\\u{202E}file");
  expect(screen.getByTestId("oracle-proposed-command").textContent).toContain(
    "go test\\u{202E} ./...",
  );
  expect(screen.getByTestId("oracle-draft-status").textContent).toContain("det\\u{200B}ail");
  expect(screen.getByText("n\\u{202E}ame_test.go")).toBeInTheDocument();
});

test("a file with invalid UTF-8 shows explicit \\xNN escapes and the approval carries the hash of the raw bytes", async () => {
  const bytes = new Uint8Array([0x67, 0x6f, 0x80, 0xff, 0x0a]);
  const server = oracleServer({ "RUN_COMMAND.txt": bytes });
  const { onApprove } = setup(server);
  await loaded();
  await toggle(panel(), "RUN_COMMAND.txt");

  await waitFor(() => {
    expect(contentOf("RUN_COMMAND.txt")).toBe("go\\x80\\xFF\n");
  });
  expect(screen.queryByText(/�/)).toBeNull();
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });
  await userEvent.click(approveButton());
  expect(onApprove).toHaveBeenCalledWith({ "oracle/RUN_COMMAND.txt": sha256HexBytes(bytes) });
});

test("a failed Reload keeps the stale listing visible with its error and disables Approve until a reload succeeds", async () => {
  const server = oracleServer({ "RUN_COMMAND.txt": "go test ./...\n" });
  setup(server);
  await loaded();
  await toggle(panel(), "RUN_COMMAND.txt");
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });

  server.failListing = true;
  await userEvent.click(screen.getByRole("button", { name: "Reload files" }));
  expect(await screen.findByTestId("oracle-stale-listing")).toBeInTheDocument();
  expect(screen.getByText("RUN_COMMAND.txt")).toBeInTheDocument();
  expect(approveButton()).toBeDisabled();

  server.failListing = false;
  await userEvent.click(screen.getByRole("button", { name: "Reload files" }));
  await waitFor(() => {
    expect(screen.queryByTestId("oracle-stale-listing")).toBeNull();
  });
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });
});

test("without an override token there is no Edit and Approve stays disabled", async () => {
  setup(oracleServer(FILES), { writesEnabled: false });
  await loaded();
  await toggle(panel(), "RUN_COMMAND.txt");
  await screen.findByTestId("oracle-content-RUN_COMMAND.txt");
  expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
  for (const name of ["a_oracle_test.go", "MANIFEST.json"]) {
    await toggle(panel(), name);
    await screen.findByTestId(`oracle-content-${name}`);
  }
  expect(approveButton()).toBeDisabled();
});

test("shows the drafting status, its detail and the proposed command, and keeps Request changes available", async () => {
  // "Request changes" belongs to the request detail screen, not this panel.
  const server = oracleServer(FILES);
  server.draft = {
    status: "failed",
    detail: "model returned no manifest",
    proposed_command: "go test ./internal/a/...",
  };
  setup(server);

  expect(
    await screen.findByText(/Oracle draft: Drafting failed -- model returned no manifest/),
  ).toBeInTheDocument();
  expect(screen.getByTestId("oracle-proposed-command")).toBeInTheDocument();
  expect(
    screen.getByText("Suggested RUN_COMMAND.txt (a suggestion only -- not written to the file):"),
  ).toBeInTheDocument();
});

test("oracle_drafting shows an in-progress notice, and the previous pass's failure when there was one", () => {
  renderApp(
    <OracleDraftingSection
      request={requestSummary({
        state: "oracle_drafting",
        oracle_draft: { status: "failed", detail: "timeout" },
      })}
    />,
  );

  expect(screen.getByTestId("oracle-drafting-section")).toBeInTheDocument();
  expect(screen.getByText(/Drafting acceptance-test oracles/)).toBeInTheDocument();
  expect(screen.getByText(/Previous pass: Drafting failed -- timeout/)).toBeInTheDocument();
  expect(screen.queryByTestId("oracle-review-panel")).toBeNull();
  expect(screen.queryByRole("button", { name: /^Approve/ })).toBeNull();
});

test("shows a per-criterion eligibility verdict alongside the drafting status", async () => {
  const server = oracleServer(FILES);
  server.draft = { status: "none_eligible", detail: "no criterion was testable" };
  setup(server, {
    request: requestSummary({
      oracle_draft: {
        status: "none_eligible",
        detail: "no criterion was testable",
        criteria: [
          {
            number: 1,
            eligible: false,
            reason: "divide_numbers(a, 0) raises ValueError, not a return value",
          },
          { number: 2, eligible: true, reason: "pure function, deterministic" },
        ],
      },
    }),
  });

  expect(await screen.findByTestId("oracle-draft-criteria")).toBeInTheDocument();
  expect(
    screen.getByText(
      "1. Not eligible -- divide_numbers(a, 0) raises ValueError, not a return value",
    ),
  ).toBeInTheDocument();
  expect(screen.getByText("2. Eligible -- pure function, deterministic")).toBeInTheDocument();
});

test("the file tile marks a file as shown only while it counts", async () => {
  setup(oracleServer({ "a_oracle_test.go": "package a\n" }));
  await loaded();
  expect(tileOf(panel(), "a_oracle_test.go")).toHaveAttribute("data-shown", "false");
  await toggle(panel(), "a_oracle_test.go");
  await waitFor(() => {
    expect(tileOf(panel(), "a_oracle_test.go")).toHaveAttribute("data-shown", "true");
  });
});

test("a failed first listing shows the error with Retry, and Retry recovers", async () => {
  const server = oracleServer({ "a_oracle_test.go": "package a\n" });
  server.failListing = true;
  setup(server);
  expect(await screen.findByRole("alert")).toBeInTheDocument();
  server.failListing = false;
  await userEvent.click(screen.getByRole("button", { name: "Retry" }));
  expect(await screen.findByRole("button", { name: "Reload files" })).toBeInTheDocument();
});
