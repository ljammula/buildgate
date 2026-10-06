package composeservices

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SynthesizeOptions carries the factoryd-side resource defaults Synthesize
// stamps onto every service -- these never come from the target repo's own
// compose file (ParseFile's allow-list has no field for any of them; see
// its own rejectedFields entries for PidsLimit/CPUS/SecurityOpt), so a
// synthesized project's containment can never be loosened by content a
// sandboxed worker itself writes. The one exception, a service's own
// mem_limit, can only narrow MemoryLimit (see ServiceSpec.MemLimit).
type SynthesizeOptions struct {
	// MemoryLimit is a Docker-accepted memory amount, e.g. "2g". Applied to
	// every service as both mem_limit and memswap_limit -- an equal
	// mem_limit/memswap_limit disables swap for the container rather than
	// giving it 2x its memory limit's worth of swap on top, which is
	// compose/Docker's own default when memswap_limit is left unset.
	MemoryLimit string
	// CPUs is a Docker-accepted CPU count, e.g. "1".
	CPUs string
	// PIDsLimit caps each service's own process count.
	PIDsLimit int
	// Labels are stamped onto every synthesized service's own `labels:`
	// block verbatim -- opaque to this package, never derived from the
	// target repo's own file (ParseFile's own rejectedFields entry for
	// "Labels" refuses a source-declared label outright). The caller
	// (internal/sandbox.ComposeServicesLifecycle) uses this for its own
	// ownership/traceability labels (data-dir hash and run ID) so orphan
	// reconciliation can identify a project's containers without parsing
	// them back out of the project's own name.
	Labels map[string]string
	// BindSourceDir is the lifecycle-owned directory containing bind inputs
	// materialized from the base commit. It must be absolute whenever a
	// ServiceSpec contains a bind mount; Synthesize never emits a source
	// repository path directly.
	BindSourceDir string
}

// DefaultPIDsLimit is the PIDsLimit a caller not exposing its own tuning
// knob for it (cmd/factoryd's -compose-services surface deliberately has
// none -- see sandbox.LoadComposeServicesSpecFromGit's own doc comment)
// stamps onto every synthesized service, mirroring DefaultMaxServices'
// own role for Options.MaxServices.
const DefaultPIDsLimit = 256

// Synthesize builds a minimal, valid compose project file from services --
// the allow-listed output of ParseFile, never the source document itself --
// containing only the fields this package's own allow-list already vetted,
// plus the resource ceilings in opts. It is the mirror image of ParseFile:
// where ParseFile turns an untrusted compose file into a small, safe set of
// facts, Synthesize turns those facts back into a compose file the real
// `docker compose` CLI can run, without ever re-introducing a field
// ParseFile itself would have rejected.
//
// networkName is the pre-created, factory-owned Docker network (created
// with --internal by the caller, mirroring internal/sandbox/relay.go and
// internal/sandbox/registryproxy.go's own internal per-run networks) every
// service is placed on -- the top-level `networks:` block pins to it via
// `external: true`, so `docker compose up` never creates its own bridge
// network for this project regardless of what the source file's own
// (rejected) top-level networks section might have asked for.
//
// depends_on's condition is carried straight over from ServiceSpec.
// DependsOn -- ParseFile already validated it against the three conditions
// the compose spec documents, so Synthesize never re-derives one from
// healthcheck presence (see ServiceSpec.DependsOn's own doc comment).
//
// Every literal "$" in an environment value is re-escaped here as "$$":
// ParseFile's own decodeEnvironment doc comment confirms compose-go's
// interpolation step decodes "$$" in the *source* YAML down to a literal
// "$" by the time ParseFile ever sees it, so a value that legitimately
// contains "$" only round-trips correctly if this reverses that exact
// substitution before writing a new YAML document — otherwise the real
// `docker compose` CLI that later loads this synthesized file would try to
// interpolate it a second time, against ITS OWN environment (which
// Synthesize has no control over and ParseFile deliberately never reads
// from, see ParseFile's package doc comment), not the target repo's.
func Synthesize(services []ServiceSpec, networkName string, opts SynthesizeOptions) ([]byte, error) {
	if networkName == "" {
		return nil, fmt.Errorf("synthesize compose project: networkName must not be empty")
	}

	namedVolumes := map[string]bool{}
	yamlServices := make(map[string]any, len(services))
	names := make([]string, 0, len(services))
	for _, svc := range services {
		names = append(names, svc.Name)
	}
	sort.Strings(names) // deterministic output regardless of caller-supplied order

	byName := make(map[string]ServiceSpec, len(services))
	for _, svc := range services {
		byName[svc.Name] = svc
	}

	for _, name := range names {
		svc := byName[name]
		// The operator's amount verbatim unless the service declared its
		// own, narrower mem_limit (ParseFile allows only that direction;
		// EffectiveMemoryBytes takes the min again rather than trust it).
		var memLimit any = opts.MemoryLimit
		if svc.MemLimit > 0 {
			bytes, err := EffectiveMemoryBytes(svc, opts.MemoryLimit)
			if err != nil {
				return nil, fmt.Errorf("synthesize compose project: service %q: %w", name, err)
			}
			memLimit = bytes
		}
		out := map[string]any{
			"image":         svc.Image,
			"mem_limit":     memLimit,
			"memswap_limit": memLimit,
			"cpus":          opts.CPUs,
			"pids_limit":    opts.PIDsLimit,
			"security_opt":  []string{"no-new-privileges:true"},
			// "default", the top-level networks key, not networkName
			// itself: the top-level default block's own "name:" is what
			// binds that key to the real, pre-created external network
			// (see below) -- a service can only ever reference a network
			// by its key within this document, never by the external
			// network's real Docker-side name directly.
			"networks": []string{"default"},
		}
		if svc.Platform != "" {
			out["platform"] = svc.Platform
		}
		if len(opts.Labels) > 0 {
			out["labels"] = opts.Labels
		}
		if svc.Hostname != "" {
			out["hostname"] = svc.Hostname
		}
		if len(svc.Environment) > 0 {
			out["environment"] = escapeEnvironment(svc.Environment)
		}
		// nil-checked, not len-checked: svc.Command/svc.Entrypoint being
		// non-nil (even empty) means the source file explicitly set
		// "command: []"/"entrypoint: []" to clear the image's own default,
		// which must round-trip as an explicit empty list here rather than
		// being omitted like an unset field would be -- see ServiceSpec.
		// Entrypoint's own doc comment.
		if svc.Command != nil {
			out["command"] = svc.Command
		}
		if svc.Entrypoint != nil {
			out["entrypoint"] = svc.Entrypoint
		}
		if svc.User != "" {
			out["user"] = svc.User
		}
		if svc.WorkingDir != "" {
			out["working_dir"] = svc.WorkingDir
		}
		if svc.ReadOnly {
			out["read_only"] = true
		}
		if svc.Init {
			out["init"] = true
		}
		if svc.ShmSize > 0 {
			out["shm_size"] = svc.ShmSize
		}
		if svc.Healthcheck != nil {
			if svc.Healthcheck.Disabled {
				// Compose's own convention for "explicitly disabled": never
				// alongside a test, interval, timeout, or retries -- see
				// ServiceSpec.Healthcheck's own doc comment on why this is
				// distinct from omitting the field entirely.
				out["healthcheck"] = map[string]any{"disable": true}
			} else {
				hc := map[string]any{"test": svc.Healthcheck.Test}
				if svc.Healthcheck.Interval != "" {
					hc["interval"] = svc.Healthcheck.Interval
				}
				if svc.Healthcheck.Timeout != "" {
					hc["timeout"] = svc.Healthcheck.Timeout
				}
				if svc.Healthcheck.Retries != 0 {
					hc["retries"] = svc.Healthcheck.Retries
				}
				out["healthcheck"] = hc
			}
		}
		if len(svc.DependsOn) > 0 {
			dependsOn := make(map[string]any, len(svc.DependsOn))
			for _, dep := range svc.DependsOn {
				dependsOn[dep.Name] = map[string]any{"condition": dep.Condition}
			}
			out["depends_on"] = dependsOn
		}
		// Named volumes and tmpfs mounts are split back apart here, having
		// both been reduced to the same VolumeMount shape by ParseFile: a
		// tmpfs mount becomes a real `tmpfs:` entry (never a disk-backed
		// named volume), and a named volume keeps its own source name when
		// the original file gave it one -- registering that same name in
		// namedVolumes below is what lets two services that intentionally
		// share one named volume still share it in the synthesized file,
		// rather than each getting an unshared volume of their own. An
		// anonymous volume (no source name in the original file) falls
		// back to volumeNameFor exactly as every named volume once did.
		volumeMounts := make([]string, 0, len(svc.NamedVolumes))
		tmpfsMounts := append([]string(nil), svc.Tmpfs...)
		for _, vol := range svc.NamedVolumes {
			if vol.Type == "tmpfs" {
				tmpfsMounts = append(tmpfsMounts, vol.Target)
				continue
			}
			if vol.Type == "bind" {
				if opts.BindSourceDir == "" || !filepath.IsAbs(opts.BindSourceDir) {
					return nil, fmt.Errorf("service %q bind mount %q requires an absolute materialized source directory", name, vol.Source)
				}
				source := filepath.Join(opts.BindSourceDir, filepath.FromSlash(vol.Source))
				rel, err := filepath.Rel(opts.BindSourceDir, source)
				if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return nil, fmt.Errorf("service %q bind mount source %q escapes the materialized source directory", name, vol.Source)
				}
				volumeMounts = append(volumeMounts, filepath.ToSlash(source)+":"+vol.Target+":ro")
				continue
			}
			volumeName := vol.Source
			if volumeName == "" {
				volumeName = volumeNameFor(name, vol.Target)
			}
			namedVolumes[volumeName] = true
			mount := volumeName + ":" + vol.Target
			if vol.ReadOnly {
				mount += ":ro"
			}
			volumeMounts = append(volumeMounts, mount)
		}
		if len(volumeMounts) > 0 {
			out["volumes"] = volumeMounts
		}
		if len(tmpfsMounts) > 0 {
			out["tmpfs"] = tmpfsMounts
		}
		yamlServices[name] = out
	}

	doc := map[string]any{
		"services": yamlServices,
		"networks": map[string]any{
			"default": map[string]any{"external": true, "name": networkName},
		},
	}
	if len(namedVolumes) > 0 {
		volumeNames := make([]string, 0, len(namedVolumes))
		for name := range namedVolumes {
			volumeNames = append(volumeNames, name)
		}
		sort.Strings(volumeNames)
		volumes := make(map[string]any, len(volumeNames))
		for _, name := range volumeNames {
			volumes[name] = map[string]any{}
		}
		doc["volumes"] = volumes
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode synthesized compose project: %w", err)
	}
	return out, nil
}

// volumeNameFor derives a stable, collision-free top-level volume name for
// one service's named-volume target -- two services may each declare a
// volume mounted at the same container path (e.g. "/data"), which would
// otherwise collide if this just used the target path alone as the
// top-level volume name.
func volumeNameFor(serviceName, target string) string {
	sanitized := strings.NewReplacer("/", "-", ":", "-").Replace(strings.TrimPrefix(target, "/"))
	return serviceName + "-" + sanitized
}

// escapeEnvironment reverses ParseFile's own "$$" -> "$" decoding (see this
// function's own callers and Synthesize's doc comment) so a value round-
// trips through a fresh `docker compose` load unchanged.
func escapeEnvironment(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = strings.ReplaceAll(v, "$", "$$")
	}
	return out
}
