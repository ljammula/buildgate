package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"buildgate/internal/projectconfig"
	"buildgate/internal/request"
	"buildgate/internal/sessionconfig"
)

// designGuideFileName is the staged guide part's name inside a drafting
// job's scratch directory.
const designGuideFileName = "design-guide.md"

// draftInputFiles are the optional files a drafting script reads into its
// prompt, each a path the script can open (in-container for a sandboxed
// job) or "" when the job has none. They are staged inside the job's
// scratch directory, like its output and evidence files, not through
// runSandboxWithRetries' specPath/extraRunInput staging: plan_tickets.py's
// --spec/--request pair already occupies both of those slots, and the
// scratch directory already sits inside the read-write /workspace mount.
type draftInputFiles struct {
	feedback      string // --feedback-file: a review rejection's notes
	designGuide   string // --design-guide-file: this job's part of the team design guide
	previousDraft string // --previous-draft-file: a handed-over document to revise
}

func (f draftInputFiles) args() []string {
	var args []string
	if f.feedback != "" {
		args = append(args, "--feedback-file", f.feedback)
	}
	if f.designGuide != "" {
		args = append(args, "--design-guide-file", f.designGuide)
	}
	if f.previousDraft != "" {
		args = append(args, "--previous-draft-file", f.previousDraft)
	}
	return args
}

// inContainer maps files staged in a drafting job's scratch directory on
// the host to the same files under the sandbox's mount of that directory.
func (f draftInputFiles) inContainer(containerScratchDir string) draftInputFiles {
	at := func(hostPath string) string {
		if hostPath == "" {
			return ""
		}
		return containerScratchDir + "/" + filepath.Base(hostPath)
	}
	return draftInputFiles{feedback: at(f.feedback), designGuide: at(f.designGuide), previousDraft: at(f.previousDraft)}
}

// Which part of the guide a drafting job is given: spec drafting gets the
// questions, planning gets the structural rules, and neither sees the other.
type designGuidePart int

const (
	designGuideSpecPart designGuidePart = iota
	designGuidePlanPart
)

// stageRequestDesignGuide loads the design guide the repository at
// workspace names in its committed .factory.yml and writes the chosen part
// into scratchDir for the drafting script's --design-guide-file, the same
// way a rejection's feedback is staged. It returns (nil, "", nil) when the
// repository names no guide. A named guide that does not resolve fails the
// job: drafting without the team's rules would look like a normal draft.
func stageRequestDesignGuide(workspace string, settings sessionconfig.Settings, scratchDir, sandboxUser string, part designGuidePart) (*sessionconfig.DesignGuide, string, error) {
	project, _, err := projectconfig.Load(workspace)
	if err != nil {
		return nil, "", fmt.Errorf("load %s: %w", projectconfig.FileName, err)
	}
	if project == nil || project.DesignGuide == "" {
		return nil, "", nil
	}
	guide, err := sessionconfig.LoadDesignGuide(settings, project.DesignGuide)
	if err != nil {
		return nil, "", fmt.Errorf("%s names design_guide %q: %w", projectconfig.FileName, project.DesignGuide, err)
	}
	text := guide.SpecDecisions
	if part == designGuidePlanPart {
		text = guide.PlanRules
	}
	if err := ensureRequestJobScratchDir(scratchDir, sandboxUser); err != nil {
		return nil, "", fmt.Errorf("create drafting scratch dir: %w", err)
	}
	path := filepath.Join(scratchDir, designGuideFileName)
	if err := writeRequestJobFeedbackFile(path, []byte(text+"\n"), sandboxUser); err != nil {
		return nil, "", fmt.Errorf("stage design guide: %w", err)
	}
	return guide, path, nil
}

// withDesignGuide records guide on a drafting job's spend record, so the
// request shows which guide version shaped the draft. Either may be nil.
func withDesignGuide(spend *request.JobSpend, guide *sessionconfig.DesignGuide) *request.JobSpend {
	if spend == nil || guide == nil {
		return spend
	}
	spend.DesignGuide = guide.Name
	spend.DesignGuideSHA256 = guide.SHA256
	return spend
}

// doctorCheckDesignGuide validates design_guide_dirs and, when -workspace
// names a repository, resolves the guide its .factory.yml selects the way a
// drafting job would, so a missing or malformed guide shows here and not
// at submit. Without a workspace it lists the guides the folders hold.
func doctorCheckDesignGuide(in doctorInputs) doctorCheck {
	name := "design guide"
	available, err := sessionconfig.DesignGuideNames(in.settings)
	if err != nil {
		return doctorCheck{Name: name, Err: err, Fix: "fix design_guide_dirs in your session config"}
	}
	if in.workspace == "" {
		if len(available) == 0 {
			return doctorCheck{Name: name, Detail: "none configured"}
		}
		return doctorCheck{Name: name, Detail: "available: " + strings.Join(available, ", ")}
	}
	project, _, err := projectconfig.Load(in.workspace)
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("load %s: %w", projectconfig.FileName, err)}
	}
	if project == nil || project.DesignGuide == "" {
		return doctorCheck{Name: name, Detail: "the repository names none (" + projectconfig.FileName + " design_guide)"}
	}
	guide, err := sessionconfig.LoadDesignGuide(in.settings, project.DesignGuide)
	if err != nil {
		return doctorCheck{Name: name, Err: err, Fix: "put " + project.DesignGuide + ".md, with its two headings, in a design_guide_dirs folder"}
	}
	return doctorCheck{Name: name, Detail: fmt.Sprintf("%s (%s, sha256 %s)", guide.Name, guide.Path, shortSHA256(guide.SHA256))}
}
