package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseDockerContextsReadsEachLine(t *testing.T) {
	out := []byte(`{"Current":false,"Description":"colima","DockerEndpoint":"unix:///Users/me/.colima/default/docker.sock","Error":"","Name":"colima"}
{"Current":true,"Description":"Current DOCKER_HOST based configuration","DockerEndpoint":"unix:///var/run/docker.sock","Error":"","Name":"default"}
not json
`)
	got := parseDockerContexts(out)
	if len(got) != 2 || got[0].Name != "colima" || got[0].Current || got[1].Name != "default" || !got[1].Current || got[1].Endpoint != "unix:///var/run/docker.sock" {
		t.Fatalf("parseDockerContexts = %+v", got)
	}
}

func TestDockerUnreachableCauseNamesTheFix(t *testing.T) {
	colima := dockerContext{Name: "colima", Endpoint: "unix:///Users/me/.colima/default/docker.sock", Answers: true}
	colimaStopped := dockerContext{Name: "colima-work", Endpoint: "unix:///Users/me/.colima/work/docker.sock"}
	other := dockerContext{Name: "default", Endpoint: "unix:///var/run/docker.sock", Current: true}
	cases := []struct {
		name     string
		facts    dockerContextFacts
		ok       bool
		cause    string
		fix      string
		notInFix string
	}{
		{
			name:  "colima answers on a context that is not current",
			facts: dockerContextFacts{Listed: true, ColimaInstalled: true, Contexts: []dockerContext{colima, other}},
			ok:    true, cause: `current context is "default"`, fix: "`docker context use colima`", notInFix: "brew",
		},
		{
			name:  "DOCKER_HOST overrides the context",
			facts: dockerContextFacts{Listed: true, ColimaInstalled: true, DockerHost: "tcp://10.0.0.1:2375", Contexts: []dockerContext{colima, other}},
			ok:    true, cause: "DOCKER_HOST=tcp://10.0.0.1:2375", fix: "`unset DOCKER_HOST`",
		},
		{
			name:  "DOCKER_CONTEXT overrides the stored context",
			facts: dockerContextFacts{Listed: true, ColimaInstalled: true, DockerContextEnv: "default", Contexts: []dockerContext{colima, other}},
			ok:    true, cause: "DOCKER_CONTEXT=default", fix: "`unset DOCKER_CONTEXT`",
		},
		{
			name: "the current colima is stopped while another profile's answers",
			facts: dockerContextFacts{Listed: true, ColimaInstalled: true, Contexts: []dockerContext{
				{Name: "colima", Endpoint: colima.Endpoint, Current: true},
				{Name: "colima-work", Endpoint: colimaStopped.Endpoint, Answers: true},
			}},
			ok: true, cause: "profile default", fix: "`colima start`, then `docker context use colima`", notInFix: "colima-work",
		},
		{
			name: "the default profile's context is preferred over another answering one",
			facts: dockerContextFacts{Listed: true, ColimaInstalled: true, Contexts: []dockerContext{
				{Name: "colima-work", Endpoint: colimaStopped.Endpoint, Answers: true}, colima, other,
			}},
			ok: true, cause: `answers on context "colima"`, fix: "`docker context use colima`", notInFix: "colima-work",
		},
		{
			name:  "colima's own context does not answer",
			facts: dockerContextFacts{Listed: true, ColimaInstalled: true, Contexts: []dockerContext{other, colimaStopped}},
			ok:    true, cause: "profile work", fix: "`colima start --profile work`, then `docker context use colima-work`",
		},
		{
			name:  "no colima context at all",
			facts: dockerContextFacts{Listed: true, ColimaInstalled: true, Contexts: []dockerContext{other}},
			ok:    true, cause: "no docker context is colima's", fix: "`colima start`",
		},
		{
			name:  "colima not installed",
			facts: dockerContextFacts{Listed: true, Contexts: []dockerContext{other}},
		},
		{
			name:  "no context list (no docker CLI)",
			facts: dockerContextFacts{ColimaInstalled: true},
		},
		{
			name:  "the current context is colima's and answers",
			facts: dockerContextFacts{Listed: true, ColimaInstalled: true, Contexts: []dockerContext{{Name: "colima", Endpoint: colima.Endpoint, Current: true, Answers: true}}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cause, fix, ok := dockerUnreachableCause(c.facts)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (cause %q, fix %q)", ok, c.ok, cause, fix)
			}
			if !strings.Contains(cause, c.cause) || !strings.Contains(fix, c.fix) {
				t.Errorf("cause %q, fix %q; want them to contain %q and %q", cause, fix, c.cause, c.fix)
			}
			if c.notInFix != "" && strings.Contains(fix, c.notInFix) {
				t.Errorf("fix %q names %q", fix, c.notInFix)
			}
		})
	}
}

func TestExplainDockerUnreachableReplacesOnlyTheFailedDockerRow(t *testing.T) {
	dp := newTestDeps(t)
	fakeDockerOf(dp).contextFactsFn = func(context.Context, string) dockerContextFacts {
		return dockerContextFacts{Listed: true, ColimaInstalled: true, Contexts: []dockerContext{
			{Name: "colima", Endpoint: "unix:///Users/me/.colima/default/docker.sock", Answers: true},
			{Name: "default", Endpoint: "unix:///var/run/docker.sock", Current: true},
		}}
	}
	checks := []doctorCheck{
		{Name: doctorDockerReachableName, Err: errors.New("docker version: no daemon"), Fix: dockerReachableFix()},
		{Name: "sandbox image present", Err: errors.New("not present locally"), Fix: "make install"},
	}
	got := explainDockerUnreachable(t.Context(), dp, "docker", "", checks)
	if got[0].Fix != "`docker context use colima`" || !strings.Contains(got[0].Err.Error(), "no daemon") {
		t.Errorf("docker row = %v / %q, want the context fix and the CLI's own error kept", got[0].Err, got[0].Fix)
	}
	if got[1].Fix != "make install" {
		t.Errorf("another row's fix changed to %q", got[1].Fix)
	}
	asked := false
	fakeDockerOf(dp).contextFactsFn = func(context.Context, string) dockerContextFacts { asked = true; return dockerContextFacts{} }
	explainDockerUnreachable(t.Context(), dp, "docker", "", []doctorCheck{{Name: doctorDockerReachableName}})
	if asked {
		t.Error("contexts were asked for although Docker answered")
	}
}

func dockerDownChecks() []doctorCheck {
	return []doctorCheck{
		{Name: doctorDockerReachableName, Err: errors.New("no daemon"), Fix: "`docker context use colima`"},
		needingDocker(doctorCheck{Name: "sandbox image present (localhost:5050/w@sha256:abc)", Err: errors.New("not present locally")})[0],
		{Name: "models.luna: price: ok"},
		needingDocker(doctorCheck{Name: "Temporal (localhost:7233)", Err: errors.New("not reachable")})[0],
		needingDocker(doctorCheck{Name: "memory limits: max_parallel_jobs 2", Err: errors.New("docker info"), Advisory: true})[0],
		needingDocker(doctorCheck{Name: "OpenShell gateway healthy"})[0],
		{Name: "gate token", Err: errors.New("expired"), Advisory: true},
		{Name: "release policy", Err: errors.New("denies every PR")},
	}
}

func TestDoctorReportCollapsesWhatNeedsDocker(t *testing.T) {
	var out bytes.Buffer
	failed := doctorReport(dockerDownChecks(), &out, false)
	text := out.String()
	if failed != 2 {
		t.Errorf("failed = %d, want 2 (Docker and the release policy):\n%s", failed, text)
	}
	for _, want := range []string{
		"FAIL  docker daemon reachable: no daemon\n      fix: `docker context use colima`\nskip  3 checks need Docker: sandbox image present, Temporal, memory limits\n",
		"warn  gate token: expired",
		"FAIL  release policy: denies every PR",
		"2/8 checks passed (1 warning(s), 3 need Docker); `factoryd doctor -v` lists every check",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"ok    ", "not present locally", "Temporal (localhost:7233)"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("report has %q:\n%s", unwanted, text)
		}
	}
}

func TestDoctorReportVerboseListsEveryRow(t *testing.T) {
	var out bytes.Buffer
	failed := doctorReport(dockerDownChecks(), &out, true)
	text := out.String()
	if failed != 4 {
		t.Errorf("failed = %d, want every failing row counted with -v:\n%s", failed, text)
	}
	for _, want := range []string{"ok    models.luna: price: ok", "FAIL  Temporal (localhost:7233): not reachable", "ok    OpenShell gateway healthy"} {
		if !strings.Contains(text, want) {
			t.Errorf("verbose report lacks %q:\n%s", want, text)
		}
	}
}

func TestDoctorReportWithDockerUpListsDockerRowsAsUsual(t *testing.T) {
	checks := dockerDownChecks()
	checks[0] = doctorCheck{Name: doctorDockerReachableName}
	var out bytes.Buffer
	failed := doctorReport(checks, &out, false)
	text := out.String()
	if failed != 3 || strings.Contains(text, "skip") || !strings.Contains(text, "FAIL  Temporal (localhost:7233): not reachable") {
		t.Errorf("failed = %d, want the Docker rows reported one by one:\n%s", failed, text)
	}
}
