package meter

import (
	"context"
	"maps"
	"slices"
	"strings"

	pb "buildgate/internal/meter/middlewarepb"
)

// EvaluateHttpRequest is the request side of the meter, in the relay's
// order: the config and sandbox name, the method and size caps, the ceilings
// (with every reserved estimate counted), the windows and the request rate.
// An admitted request has its body rewritten when the policy says so and
// always gets identity content-encoding, so the response side can read
// usage.
func (s *SupervisorMiddleware) EvaluateHttpRequest(_ context.Context, req *pb.HttpRequestEvaluation) (*pb.HttpRequestResult, error) {
	policy, err := DecodePolicy(req.GetConfig())
	if err != nil {
		return denyRequest(CodeInvalidConfig, err.Error()), nil
	}
	if code, reason := screenRequest(policy, req); code != "" {
		return denyRequest(code, reason), nil
	}
	entry, err := s.registry.entry(req.GetContext().GetSandboxId(), policy)
	if err != nil {
		s.registry.cfg.Logger.Printf("meter: %v", err)
		return denyRequest(CodeLedgerUnavailable, err.Error()), nil
	}
	body := policy.RewriteBody(req.GetBody())
	inputTokens, outputTokens := estimateTokens(body)
	decision := entry.admit(admitRequest{
		requestID: req.GetContext().GetRequestId(),
		policy:    policy,
		effort:    ParseRequestedReasoningEffort(body, policy.RequestFormat),
		stream:    ResponseIsEventStream("", body),
		estimate: pendingRequest{
			estimateInputTokens:  inputTokens,
			estimateOutputTokens: outputTokens,
			estimateCostMicroUSD: estimateCostMicroUSD(policy.Prices, inputTokens, outputTokens),
		},
	})
	if !decision.allowed {
		return denyRequest(decision.code, "request refused: "+decision.code), nil
	}
	return allowRequest(policy, body), nil
}

// screenRequest applies the checks that need no per-sandbox state and returns
// the deny code and reason of the first that fails, or "".
func screenRequest(policy Policy, req *pb.HttpRequestEvaluation) (code, reason string) {
	if policy.Sandbox != req.GetContext().GetSandbox() {
		return CodeSandboxMismatch, "config sandbox " + policy.Sandbox + " does not match the requesting sandbox"
	}
	if !strings.EqualFold(req.GetTarget().GetMethod(), "POST") {
		return CodeMethodNotAllowed, "method is not allowed"
	}
	if int64(len(req.GetBody())) > policy.MaxRequestBytes {
		return CodeRequestTooLarge, "request body is too large"
	}
	return "", ""
}

func denyRequest(code, reason string) *pb.HttpRequestResult {
	return &pb.HttpRequestResult{Decision: pb.Decision_DECISION_DENY, Reason: reason, ReasonCode: code}
}

// allowRequest is the allow result: identity encoding always; when the
// policy rewrites, the rewritten body and the headers the Copilot API
// requires of a client, written over whatever the worker sent.
func allowRequest(policy Policy, body []byte) *pb.HttpRequestResult {
	result := &pb.HttpRequestResult{
		Decision:        pb.Decision_DECISION_ALLOW,
		HeaderMutations: []*pb.HeaderMutation{overwriteHeader("accept-encoding", "identity")},
	}
	if policy.Rewrite == "" {
		return result
	}
	result.Body = body
	result.HasBody = true
	headers := CopilotExtraHeaders(body, policy.RequestFormat)
	for _, name := range slices.Sorted(maps.Keys(headers)) {
		result.HeaderMutations = append(result.HeaderMutations, overwriteHeader(strings.ToLower(name), headers[name]))
	}
	return result
}

func overwriteHeader(name, value string) *pb.HeaderMutation {
	return &pb.HeaderMutation{Operation: &pb.HeaderMutation_Write{Write: &pb.WriteHeader{
		Name:       name,
		Value:      value,
		OnExisting: pb.ExistingHeaderAction_EXISTING_HEADER_ACTION_OVERWRITE,
	}}}
}
