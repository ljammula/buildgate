package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"buildgate/internal/harness"
	"buildgate/internal/sessionconfig"
)

// setupFlags are `factoryd setup`'s flags: the model half of quickstart's.
type setupFlags struct {
	nonInteractive, reconfigure                                *bool
	configPath, dataDir, route, modelHost, modelID, credential *string
	egressCABundle, harnessName, sandboxImage                  *string
	contextWindow                                              *int
}

// newSetupFlags builds `factoryd setup`'s FlagSet in isolation from parsing,
// so USAGE_REFERENCE.md's doc-vs-flag drift test can enumerate its flags.
func newSetupFlags() (flags *flag.FlagSet, f setupFlags) {
	flags = flag.NewFlagSet("setup", flag.ContinueOnError)
	f.nonInteractive = flags.Bool("non-interactive", false, "ask nothing: a single detected login is used, anything else must be given as a flag. Also inferred when stdin is not a terminal")
	f.reconfigure = flags.Bool("reconfigure", false, "choose again even if the session config already names a model, keeping its recorded images")
	f.configPath = flags.String("config", "", "session config path or profile name; default: the active profile's config")
	f.dataDir = flags.String("data-dir", "", "data directory to record in a newly written config; default: ~/buildgate/data for a profile")
	f.route = flags.String("route", "", "model route: \"chatgpt-codex\", \"copilot\", \"anthropic\" or \"openai\" (an OpenAI-compatible endpoint); skips the model question")
	f.modelHost = flags.String("model-host", "", "bare API root (no /v1) of an OpenAI-compatible model endpoint; only with -route openai")
	f.modelID = flags.String("model-id", "", "model id to configure; default: the route's own default, or a pick from its model listing")
	f.contextWindow = flags.Int("context-window", 0, "the model's context window in tokens; with -route openai and -route copilot")
	f.credential = flags.String("credential", "", "API key or OAuth token for a credentialed route; never written to config")
	f.egressCABundle = flags.String("egress-ca-bundle", "", "PEM file trusted for the outbound TLS calls setup makes and written to the config")
	f.harnessName = flags.String("harness", harness.Pi, "coding agent every role runs: \"pi\", \"codex\" (needs -route chatgpt-codex) or \"pifork\" (needs -sandbox-image); skips the coding-agent question")
	f.sandboxImage = flags.String("sandbox-image", "", "digest-pinned pifork worker image; required with -harness pifork, refused otherwise")
	plainFlagUsage(flags)
	return flags, f
}

// setupMain implements `factoryd setup`: the questions an install cannot
// answer for the operator, and only those. Which model (detected logins
// first; not asked when -route is given) and, where the route can run more
// than one, which coding agent. Everything else is defaulted, and the
// answers are written into the session config `make install` created, so a
// run needs nothing further configured. It is quickstart's config stage
// without a repository: quickstart on a machine set up this way asks about
// the repository and the request only.
func setupMain(dp *deps, args []string) error {
	flags, f := newSetupFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	explicit := map[string]bool{}
	flags.Visit(func(fl *flag.Flag) { explicit[fl.Name] = true })
	opts := &quickstartOptions{
		NonInteractive:        *f.nonInteractive || !quickstartStdinIsInteractive(os.Stdin),
		Reconfigure:           *f.reconfigure,
		ConfigPath:            *f.configPath,
		DataDir:               *f.dataDir,
		DataDirExplicit:       explicit["data-dir"],
		Route:                 *f.route,
		ModelHost:             *f.modelHost,
		ModelID:               *f.modelID,
		ContextWindow:         *f.contextWindow,
		ContextWindowExplicit: explicit["context-window"],
		Credential:            *f.credential,
		CredentialProvided:    explicit["credential"],
		EgressCABundle:        *f.egressCABundle,
		Harness:               *f.harnessName,
		HarnessExplicit:       explicit["harness"],
		SandboxImage:          *f.sandboxImage,
	}
	if err := quickstartValidateHarness(opts); err != nil {
		return err
	}
	return runSetup(dp, opts, os.Stdin, os.Stdout)
}

func runSetup(dp *deps, opts *quickstartOptions, stdin io.Reader, w io.Writer) error {
	configPath, existing, err := quickstartResolveExistingConfig(opts)
	if err != nil {
		return err
	}
	if existing != nil && quickstartConfigHasExecutionRole(existing) {
		fmt.Fprintf(w, "Already set up: %s is configured (%s). `factoryd setup -reconfigure` chooses again.\n", configPath, setupSummary(configPath))
		return nil
	}
	if _, _, _, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(stdin), w, configPath, existing); err != nil {
		return err
	}
	fmt.Fprintf(w, "Set up: %s. Next: factoryd quickstart <repo> \"<request>\"\n", setupSummary(configPath))
	return nil
}

// setupSummary is the model and coding agent the config at path gives the
// execution role.
func setupSummary(path string) string {
	cfg, err := sessionconfig.Load(path)
	if err != nil || cfg.Roles == nil || cfg.Roles.Execution == nil {
		return "see " + path
	}
	agent := cfg.Roles.Execution.Harness
	if agent == "" {
		agent = harness.Pi
	}
	return fmt.Sprintf("model %s, coding agent %s", cfg.Roles.Execution.Model, agent)
}
