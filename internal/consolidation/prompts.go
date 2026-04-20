package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ClusterInput is what a profile receives for each candidate group:
// the short-term traces the pipeline is asking the model to possibly
// distill into a single mid-term memory. The title+body pair is
// enough context for cohesion and distillation decisions; tags ride
// along so the distillation inherits a reasonable starting set.
type ClusterInput struct {
	Traces []TraceInput
}

type TraceInput struct {
	ID    string
	Title string
	Body  string
	Tags  []string
}

// Distillation is the result of a successful pass. Confidence is in
// [0,1]; when the profile doesn't produce a confidence signal (small
// profile's template step) the value is zero and the caller treats
// it as "unknown, not zero-confidence".
type Distillation struct {
	Cohesive   bool
	Title      string
	Body       string
	Tags       []string
	Confidence float64
}

// Profile is the strategy interface — one implementation per
// model-tier bucket. Each profile owns its prompt shape and its
// response parsing so the pipeline code stays model-agnostic.
type Profile interface {
	Name() string
	MaxClusterSize() int
	Run(ctx context.Context, llm LLMClient, model string, cluster ClusterInput) (Distillation, error)
}

// GetProfile returns the profile implementation for a tier name. An
// unknown tier falls back to "large" — the conservative default —
// and logs a note so misconfigurations are visible but not fatal.
func GetProfile(tier string) Profile {
	switch tier {
	case "small":
		return smallProfile{}
	case "large":
		return largeProfile{}
	case "frontier":
		return frontierProfile{}
	default:
		return largeProfile{}
	}
}

// formatTraces builds the trace-excerpt block shared by every
// profile's prompts. Each trace gets an ID line, title, tag list,
// and body (truncated per-trace to keep context bounded on local
// models with limited windows).
func formatTraces(c ClusterInput, bodyLimit int) string {
	var sb strings.Builder
	for i, tr := range c.Traces {
		fmt.Fprintf(&sb, "--- Trace %d (id=%s) ---\n", i+1, tr.ID)
		fmt.Fprintf(&sb, "Title: %s\n", tr.Title)
		if len(tr.Tags) > 0 {
			fmt.Fprintf(&sb, "Tags: %s\n", strings.Join(tr.Tags, ", "))
		}
		body := tr.Body
		if bodyLimit > 0 && len(body) > bodyLimit {
			body = body[:bodyLimit] + "\n[…truncated]"
		}
		fmt.Fprintf(&sb, "Body:\n%s\n\n", body)
	}
	return sb.String()
}

// ---- small profile (7B-13B models): multi-step template ----

type smallProfile struct{}

func (smallProfile) Name() string        { return "small" }
func (smallProfile) MaxClusterSize() int { return 5 }

func (p smallProfile) Run(ctx context.Context, llm LLMClient, model string, cluster ClusterInput) (Distillation, error) {
	cohesive, err := runCohesionStep(ctx, llm, model, cluster, 300)
	if err != nil {
		return Distillation{}, err
	}
	if !cohesive {
		return Distillation{Cohesive: false}, nil
	}
	d, err := runTemplateStep(ctx, llm, model, cluster, 300)
	if err != nil {
		return Distillation{}, err
	}
	d.Cohesive = true
	return d, nil
}

// ---- large profile (30B-70B models): multi-step + confidence ----

type largeProfile struct{}

func (largeProfile) Name() string        { return "large" }
func (largeProfile) MaxClusterSize() int { return 10 }

func (p largeProfile) Run(ctx context.Context, llm LLMClient, model string, cluster ClusterInput) (Distillation, error) {
	cohesive, err := runCohesionStep(ctx, llm, model, cluster, 400)
	if err != nil {
		return Distillation{}, err
	}
	if !cohesive {
		return Distillation{Cohesive: false}, nil
	}
	d, err := runTemplateStep(ctx, llm, model, cluster, 400)
	if err != nil {
		return Distillation{}, err
	}
	conf, err := runConfidenceStep(ctx, llm, model, cluster)
	if err != nil {
		// Confidence is optional — proceed with zero confidence on
		// parse failures rather than bailing the whole pass.
		conf = 0
	}
	d.Cohesive = true
	d.Confidence = conf
	return d, nil
}

// ---- frontier profile: single-shot JSON ----

type frontierProfile struct{}

func (frontierProfile) Name() string        { return "frontier" }
func (frontierProfile) MaxClusterSize() int { return 20 }

func (p frontierProfile) Run(ctx context.Context, llm LLMClient, model string, cluster ClusterInput) (Distillation, error) {
	body := formatTraces(cluster, 800)
	user := fmt.Sprintf(`Below are %d short-term memories from a Noema cortex. Your job is to decide whether they belong together (same topic / same decision / same ongoing work) and, if so, to distill them into a single consolidated memory for the mid-term tier.

%s

Respond with a JSON object and nothing else:

{
  "cohesive": <true|false>,
  "title": "<=100 chars, one line, no date prefix",
  "tags": ["tag1", "tag2", ...],          // 1-8 tags
  "body": "1-3 paragraphs distilling the cluster",
  "confidence": <0.0-1.0>                  // how confident you are the distillation preserves the essential information
}

If "cohesive" is false, the other fields may be null or omitted.`, len(cluster.Traces), body)

	raw, err := llm.Complete(ctx, CompletionRequest{
		Model:       model,
		Messages:    []Message{{Role: "user", Content: user}},
		Temperature: 0.2,
		MaxTokens:   1200,
	})
	if err != nil {
		return Distillation{}, err
	}
	return parseFrontierResponse(raw)
}

// ---- shared step runners ----

func runCohesionStep(ctx context.Context, llm LLMClient, model string, cluster ClusterInput, bodyLimit int) (bool, error) {
	body := formatTraces(cluster, bodyLimit)
	user := fmt.Sprintf(`Below are %d short-term memories from a Noema cortex. Are they cohesive enough — same topic, decision, ongoing work — to consolidate into one memory?

%s

Answer with a single word on one line, with no other text: yes or no.`, len(cluster.Traces), body)

	raw, err := llm.Complete(ctx, CompletionRequest{
		Model:       model,
		Messages:    []Message{{Role: "user", Content: user}},
		Temperature: 0.0,
		MaxTokens:   16,
	})
	if err != nil {
		return false, err
	}
	return parseCohesion(raw), nil
}

func runTemplateStep(ctx context.Context, llm LLMClient, model string, cluster ClusterInput, bodyLimit int) (Distillation, error) {
	body := formatTraces(cluster, bodyLimit)
	user := fmt.Sprintf(`Below are %d short-term memories from a Noema cortex that are cohesive enough to consolidate. Write one consolidated memory for the mid-term tier.

%s

Fill in each field exactly. Do not add other fields, do not omit any:

Title: <one line, <=100 chars, no date prefix>
Tags: <comma-separated list, 1-8 tags>
Body: <1-3 paragraphs distilling the cluster>`, len(cluster.Traces), body)

	raw, err := llm.Complete(ctx, CompletionRequest{
		Model:       model,
		Messages:    []Message{{Role: "user", Content: user}},
		Temperature: 0.2,
		MaxTokens:   800,
	})
	if err != nil {
		return Distillation{}, err
	}
	return parseTemplate(raw)
}

func runConfidenceStep(ctx context.Context, llm LLMClient, model string, cluster ClusterInput) (float64, error) {
	user := `On a scale of 1 to 10, how confident are you that the consolidation you just wrote preserves the essential information from the source memories?

Answer with a single integer on one line, with no other text.`

	raw, err := llm.Complete(ctx, CompletionRequest{
		Model:       model,
		Messages:    []Message{{Role: "user", Content: user}},
		Temperature: 0.0,
		MaxTokens:   8,
	})
	if err != nil {
		return 0, err
	}
	return parseConfidenceInt(raw), nil
}

// ---- parsers ----

var yesRE = regexp.MustCompile(`(?i)^\s*yes\b`)
var intRE = regexp.MustCompile(`\b([0-9]|10)\b`)

func parseCohesion(raw string) bool {
	return yesRE.MatchString(strings.TrimSpace(raw))
}

func parseConfidenceInt(raw string) float64 {
	m := intRE.FindStringSubmatch(raw)
	if m == nil {
		return 0
	}
	switch m[1] {
	case "10":
		return 1.0
	default:
		// "0".."9" — map "10" separately above
		return float64(int(m[1][0]-'0')) / 10.0
	}
}

func parseTemplate(raw string) (Distillation, error) {
	var d Distillation
	lines := strings.Split(raw, "\n")
	var bodyLines []string
	inBody := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case !inBody && strings.HasPrefix(trimmed, "Title:"):
			d.Title = strings.TrimSpace(strings.TrimPrefix(trimmed, "Title:"))
		case !inBody && strings.HasPrefix(trimmed, "Tags:"):
			raw := strings.TrimSpace(strings.TrimPrefix(trimmed, "Tags:"))
			for _, t := range strings.Split(raw, ",") {
				if tag := strings.TrimSpace(t); tag != "" {
					d.Tags = append(d.Tags, tag)
				}
			}
		case !inBody && strings.HasPrefix(trimmed, "Body:"):
			inBody = true
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "Body:"))
			if rest != "" {
				bodyLines = append(bodyLines, rest)
			}
		case inBody:
			bodyLines = append(bodyLines, line)
		}
	}
	d.Body = strings.TrimSpace(strings.Join(bodyLines, "\n"))
	if d.Title == "" || d.Body == "" {
		return d, fmt.Errorf("template response missing required fields: got title=%q body-len=%d", d.Title, len(d.Body))
	}
	return d, nil
}

func parseFrontierResponse(raw string) (Distillation, error) {
	// Strip ```json fences if present — models sometimes wrap even
	// when told not to.
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	var body struct {
		Cohesive   bool     `json:"cohesive"`
		Title      string   `json:"title"`
		Tags       []string `json:"tags"`
		Body       string   `json:"body"`
		Confidence float64  `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(s), &body); err != nil {
		return Distillation{}, fmt.Errorf("frontier JSON parse failed: %w", err)
	}
	d := Distillation{
		Cohesive:   body.Cohesive,
		Title:      strings.TrimSpace(body.Title),
		Body:       strings.TrimSpace(body.Body),
		Tags:       body.Tags,
		Confidence: body.Confidence,
	}
	if d.Cohesive && (d.Title == "" || d.Body == "") {
		return d, fmt.Errorf("frontier response claims cohesive but missing title or body")
	}
	return d, nil
}
