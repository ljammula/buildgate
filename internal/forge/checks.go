package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Check is one check of a pull request head's status check rollup: a check
// run (GitHub Actions and other apps) or a commit status. It carries only
// the structured fields the factory sorts on, never a log line.
type Check struct {
	// Name is the check run's name or the commit status's context.
	Name string
	// Poster is the app slug of a check run or the context of a commit
	// status (its creator's login when it has no context), as GitHub
	// records it.
	Poster string
	// Completed is false while the check is queued, in progress or pending.
	Completed bool
	// Conclusion is a completed check run's conclusion, upper case
	// (SUCCESS, FAILURE, CANCELLED, TIMED_OUT, ...). A commit status maps
	// SUCCESS to SUCCESS, FAILURE to FAILURE and ERROR to ERROR.
	Conclusion string
	DetailsURL string
	// Required is whether GitHub's isRequired for the pull request says the
	// check is required; nil when it could not be read
	// (ReviewState.RequiredUnknown).
	Required *bool
	// RunID is the check run's database id, which for a GitHub Actions
	// check run is its job's id. Zero for a commit status.
	RunID int64
}

// Passing reports whether a completed check succeeded the way
// checksPassingFromRollup counts it.
func (c Check) Passing() bool {
	return c.Completed && checkPassingStates[c.Conclusion]
}

// checksQuery reads the checks of the pull request's head commit.
// %s is replaced by ", isRequired(pullRequestNumber: $number)" on the
// check run and commit status selections, or nothing when the required
// flag could not be read. A page holds 100 checks; further pages follow the
// cursor.
const checksQuery = `query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      commits(last: 1) {
        nodes {
          commit {
            oid
            statusCheckRollup {
              contexts(first: 100, after: $cursor) {
                pageInfo { hasNextPage endCursor }
                nodes {
                  __typename
                  ... on CheckRun {
                    name status conclusion detailsUrl databaseId%[1]s
                    checkSuite { app { slug } }
                  }
                  ... on StatusContext {
                    context state targetUrl%[1]s
                    creator { login }
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`

const isRequiredField = " isRequired(pullRequestNumber: $number)"

type ghChecksResponse struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				Commits struct {
					Nodes []struct {
						Commit struct {
							OID               string `json:"oid"`
							StatusCheckRollup *struct {
								Contexts struct {
									PageInfo struct {
										HasNextPage bool   `json:"hasNextPage"`
										EndCursor   string `json:"endCursor"`
									} `json:"pageInfo"`
									Nodes []ghCheckNode `json:"nodes"`
								} `json:"contexts"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

type ghCheckNode struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"detailsUrl"`
	DatabaseID int64  `json:"databaseId"`
	CheckSuite struct {
		App struct {
			Slug string `json:"slug"`
		} `json:"app"`
	} `json:"checkSuite"`
	Context    string `json:"context"`
	State      string `json:"state"`
	TargetURL  string `json:"targetUrl"`
	IsRequired *bool  `json:"isRequired"`
	Creator    struct {
		Login string `json:"login"`
	} `json:"creator"`
}

// parseChecks parses checksQuery's response: the head commit's oid, one page
// of its checks, and the page's cursor. A pure function over recorded JSON.
func parseChecks(data []byte) (oid string, checks []Check, hasNext bool, cursor string, err error) {
	var r ghChecksResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return "", nil, false, "", fmt.Errorf("parse checks response: %w", err)
	}
	nodes := r.Data.Repository.PullRequest.Commits.Nodes
	if len(nodes) == 0 {
		return "", nil, false, "", nil
	}
	commit := nodes[0].Commit
	if commit.StatusCheckRollup == nil {
		return commit.OID, nil, false, "", nil
	}
	ctxs := commit.StatusCheckRollup.Contexts
	for _, n := range ctxs.Nodes {
		switch n.Typename {
		case "CheckRun":
			checks = append(checks, Check{
				Name: n.Name, Poster: n.CheckSuite.App.Slug,
				Completed:  strings.EqualFold(n.Status, "COMPLETED"),
				Conclusion: strings.ToUpper(n.Conclusion),
				DetailsURL: n.DetailsURL, Required: n.IsRequired, RunID: n.DatabaseID,
			})
		case "StatusContext":
			poster := n.Context
			if poster == "" {
				poster = n.Creator.Login
			}
			completed := n.State == "SUCCESS" || n.State == "FAILURE" || n.State == "ERROR"
			conclusion := ""
			if completed {
				conclusion = n.State
			}
			checks = append(checks, Check{
				Name: n.Context, Poster: poster, Completed: completed,
				Conclusion: conclusion, DetailsURL: n.TargetURL, Required: n.IsRequired,
			})
		}
	}
	return commit.OID, checks, ctxs.PageInfo.HasNextPage, ctxs.PageInfo.EndCursor, nil
}

// readChecks reads every check of the pull request's head through
// `gh api graphql`. When the query with isRequired fails (a field error or a
// missing permission) it asks again without it and reports requiredUnknown,
// every Required nil. headOID is the commit the checks belong to.
func readChecks(ctx context.Context, ghBinary, owner, repo string, number int) (headOID string, checks []Check, requiredUnknown bool, err error) {
	headOID, checks, err = readChecksPages(ctx, ghBinary, owner, repo, number, isRequiredField)
	if err == nil {
		return headOID, checks, false, nil
	}
	headOID, checks, err2 := readChecksPages(ctx, ghBinary, owner, repo, number, "")
	if err2 != nil {
		return "", nil, false, err
	}
	return headOID, checks, true, nil
}

func readChecksPages(ctx context.Context, ghBinary, owner, repo string, number int, required string) (string, []Check, error) {
	query := fmt.Sprintf(checksQuery, required)
	var all []Check
	var headOID, cursor string
	for {
		args := []string{"api", "graphql",
			"-f", "query=" + query,
			"-F", "owner=" + owner,
			"-F", "repo=" + repo,
			"-F", "number=" + strconv.Itoa(number),
		}
		if cursor != "" {
			args = append(args, "-F", "cursor="+cursor)
		}
		out, err := exec.CommandContext(ctx, ghBinary, args...).CombinedOutput()
		if err != nil {
			return "", nil, fmt.Errorf("gh api graphql checks: %w: %s", err, strings.TrimSpace(string(out)))
		}
		oid, page, hasNext, next, err := parseChecks(out)
		if err != nil {
			return "", nil, err
		}
		headOID = oid
		all = append(all, page...)
		if !hasNext {
			return headOID, all, nil
		}
		cursor = next
	}
}

// ReadJobLog returns the log of the GitHub Actions job jobID of the
// repository prURL belongs to (`gh api repos/{owner}/{repo}/actions/jobs/{id}/logs`).
func (o GHPullRequestOpener) ReadJobLog(ctx context.Context, prURL string, jobID int64) (string, error) {
	owner, repo, _, err := parsePullRequestURL(prURL)
	if err != nil {
		return "", err
	}
	ghBinary := o.GHBinary
	if ghBinary == "" {
		ghBinary = "gh"
	}
	out, err := exec.CommandContext(ctx, ghBinary, "api",
		fmt.Sprintf("repos/%s/%s/actions/jobs/%d/logs", owner, repo, jobID)).Output()
	if err != nil {
		return "", fmt.Errorf("gh api job logs %d: %w", jobID, err)
	}
	return string(out), nil
}
