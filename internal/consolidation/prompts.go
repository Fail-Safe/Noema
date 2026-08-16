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
// profile's prompts. Each trace gets an ordinal boundary, title, tag
// list, and body (truncated per-trace to keep context bounded on local
// models with limited windows). Internal IDs stay out of model input;
// the pipeline records lineage directly from the candidate rows.
func formatTraces(c ClusterInput, bodyLimit int) string {
	var sb strings.Builder
	for i, tr := range c.Traces {
		fmt.Fprintf(&sb, "--- Trace %d ---\n", i+1)
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
	conf, err := runConfidenceStep(ctx, llm, model, cluster, d)
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

Decision and grounding rules:
- Set cohesive to true only when the memories share a specific topic, project, recurring activity, or line of investigation. Shared dates, authors, tags, or words are not enough.
- A shared word or name is superficial when it refers to different entities. For example, notes about Java the island, Java coffee, and the Java programming language are not one topic merely because they contain "Java".
- Only reference entities, projects, tools, people, or topics that actually appear in the memories above. Do not invent, add, or infer topics that are not explicitly present.
- Preserve specific named entities verbatim: skill names, tool names, identifiers, file paths, error strings, bug references, proper nouns, and numeric values.
- The title must describe what the memories are actually about, not what they might relate to. Tags should come from source tags or terms that appear literally in the source bodies.
- The title and body must state the durable memory directly. Do not discuss source formatting, tags, trace IDs, memory tiers, consolidation, or evaluation mechanics unless those are explicitly the subject of the source bodies.

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
		Model:           model,
		Messages:        []Message{{Role: "user", Content: user}},
		Temperature:     0.2,
		MaxTokens:       1200,
		DisableThinking: true,
	})
	if err != nil {
		return Distillation{}, err
	}
	return parseFrontierResponse(raw)
}

// ---- shared step runners ----

func runCohesionStep(ctx context.Context, llm LLMClient, model string, cluster ClusterInput, bodyLimit int) (bool, error) {
	body := formatTraces(cluster, bodyLimit)
	user := fmt.Sprintf(`Below are %d short-term memories from a Noema cortex.

Would a single consolidated summary of these be more useful than keeping them as separate short-term memories?

Answer yes if they share a specific common thread. Each of these counts:
- same topic, project, agent session, or line of investigation
- same recurring activity across time (multiple session summaries, multiple heartbeat checks, multiple cron logs, multiple daily status reports) — a time-series of the same activity is cohesive even when individual entries are thin
- same debugging or troubleshooting effort (even across multiple steps)

Answer no if the memories are about different subjects, even if they share superficial features like the same day, the same author, or the same tag. A cluster containing "movie notes", "architecture docs", and "shopping list" is not cohesive even if they were all created on the same Tuesday. If you'd have to write an umbrella like "various activities", "mixed updates", or "general work" to summarize them together, the answer is no.

A shared word or name is also superficial when it refers to different entities. For example, notes about Java the island, Java coffee, and the Java programming language are not one topic merely because they contain "Java".

%s

Answer with a single word on one line, with no other text: yes or no.`, len(cluster.Traces), body)

	raw, err := llm.Complete(ctx, CompletionRequest{
		Model:           model,
		Messages:        []Message{{Role: "user", Content: user}},
		Temperature:     0.0,
		MaxTokens:       16,
		DisableThinking: true,
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

Grounding rules:
- Only reference entities, projects, tools, people, or topics that actually appear in the memories above. Do not invent, add, or infer topics that are not explicitly present.
- Preserve specific named entities verbatim: skill names, tool names, identifiers, file paths, error strings, bug references, proper nouns. If a source mentions "service-integration-token-expiration" or "*.example.net_ecc TLS glob bug" or "ssh-key-troubleshooting skill", the body should name those specifics rather than flattening them to generic prose like "various skills" or "network issues". The mid-term memory loses all its value if the concrete artifacts disappear.
- The title must describe what the memories are actually about, not what they might relate to. If the cluster is about one thing (e.g. all deployment sessions), the title should name that one thing — do not add unrelated subjects to make the title sound broader.
- Tags should come from the source memories' tags or from terms that appear literally in the bodies.
- The title and body must state the durable memory directly. Do not discuss source formatting, tags, trace IDs, memory tiers, consolidation, or evaluation mechanics unless those are explicitly the subject of the source bodies.

Fill in each field exactly. Do not add other fields, do not omit any:

Title: <one line, <=100 chars, no date prefix>
Tags: <comma-separated list, 1-8 tags, each tag lowercase-kebab-case>
Body: <1-3 paragraphs distilling the cluster>

Tag format rules: each tag must be a single token in lowercase-kebab-case. Good: "mcp-server", "career-goals", "multi-agent", "fastmail-api". Bad: "MCP Server", "AI SME", "Hugging Face", "Memory Consolidation". If a concept naturally has spaces, join the words with hyphens and lowercase them. Never use spaces inside a tag.`, len(cluster.Traces), body)

	raw, err := llm.Complete(ctx, CompletionRequest{
		Model:           model,
		Messages:        []Message{{Role: "user", Content: user}},
		Temperature:     0.2,
		MaxTokens:       800,
		DisableThinking: true,
	})
	if err != nil {
		return Distillation{}, err
	}
	return parseTemplate(raw)
}

func runConfidenceStep(ctx context.Context, llm LLMClient, model string, cluster ClusterInput, d Distillation) (float64, error) {
	// Each profile step is a fresh completion with no conversation
	// history, so the confidence step has to be shown both the source
	// cluster and the distillation it's being asked to rate —
	// otherwise the model is evaluating thin air and returns
	// arbitrary low numbers.
	//
	// Calibrated scale with anchors because models otherwise default
	// to "10" on every judgment. The anchors force the model to
	// distinguish between "all key information preserved" (a strong
	// distillation) and "most but some fidelity lost" (typical) and
	// "umbrella summary only" (weak). The ask for a one-sentence
	// justification before the number is a well-known anti-anchoring
	// trick — a model that has to justify "10" usually notices it's
	// not warranted and writes "7" instead.
	sources := formatTraces(cluster, 300)
	user := fmt.Sprintf(`You just wrote this consolidation:

Title: %s
Tags: %s
Body: %s

From these source memories:

%s

Rate how well the consolidation preserves the source information on this calibrated scale:

  10 = every specific fact, name, and detail from every source is preserved
  7-9 = all key points preserved, minor details omitted
  4-6 = general theme preserved, specific facts lost
  1-3 = only a vague umbrella description

Be strict. Most summaries fall in the 4-8 range.

Answer in exactly this format, nothing else:
<one-sentence justification>
<integer 1-10>`, d.Title, strings.Join(d.Tags, ", "), d.Body, sources)

	raw, err := llm.Complete(ctx, CompletionRequest{
		Model:           model,
		Messages:        []Message{{Role: "user", Content: user}},
		Temperature:     0.0,
		MaxTokens:       120,
		DisableThinking: true,
	})
	if err != nil {
		return 0, err
	}
	return parseConfidenceInt(raw), nil
}

// normalizeTags coerces LLM-generated tag strings into the kebab-case
// shape the cortex expects. Local models ignore "use hyphens" even
// when the prompt says so, emitting phrases like "MCP Server" or
// "career goals"; without this, those phrases land in the DB as-is
// and render struck-through in Obsidian's tag pane. Rules:
//   - lowercase
//   - whitespace runs collapse to a single hyphen
//   - drop any character that isn't [a-z0-9-_]
//   - trim leading/trailing hyphens
//   - drop empty or single-character results (defensive)
//   - deduplicate, preserving the first occurrence
func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, raw := range tags {
		norm := normalizeTag(raw)
		if len(norm) < 2 {
			continue
		}
		if _, ok := seen[norm]; ok {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	return out
}

func normalizeTag(raw string) string {
	var sb strings.Builder
	sb.Grow(len(raw))
	lastWasHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_':
			sb.WriteRune(r)
			lastWasHyphen = false
		case r == '-' || r == ' ' || r == '\t' || r == '/' || r == '.' || r == ',':
			if !lastWasHyphen && sb.Len() > 0 {
				sb.WriteRune('-')
				lastWasHyphen = true
			}
		}
	}
	return strings.TrimRight(sb.String(), "-")
}

// ---- parsers ----

var yesRE = regexp.MustCompile(`(?i)^\s*yes\b`)
var intRE = regexp.MustCompile(`\b([0-9]|10)\b`)

func parseCohesion(raw string) bool {
	return yesRE.MatchString(strings.TrimSpace(raw))
}

func parseConfidenceInt(raw string) float64 {
	// With the calibrated-scale prompt the model writes a
	// justification sentence before the integer, so the score is the
	// LAST integer in the response, not the first. FindAllStringSubmatch
	// gives us every match in order; we take the final one.
	all := intRE.FindAllStringSubmatch(raw, -1)
	if len(all) == 0 {
		return 0
	}
	last := all[len(all)-1][1]
	if last == "10" {
		return 1.0
	}
	return float64(int(last[0]-'0')) / 10.0
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
	if err := validateDistillationShape(d); err != nil {
		return d, fmt.Errorf("template response: %w", err)
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
	if d.Cohesive {
		if err := validateDistillationShape(d); err != nil {
			return d, fmt.Errorf("frontier response claims cohesive but %w", err)
		}
	}
	return d, nil
}

func validateDistillationShape(d Distillation) error {
	if d.Title == "" || d.Body == "" {
		return fmt.Errorf("missing required fields: got title=%q body-len=%d", d.Title, len(d.Body))
	}
	if len([]rune(d.Title)) > 100 {
		return fmt.Errorf("title exceeds 100 characters")
	}
	lowerTitle := strings.ToLower(d.Title)
	if strings.Contains(lowerTitle, "tags:") || strings.Contains(lowerTitle, "body:") {
		return fmt.Errorf("title contains an inline field label")
	}
	if len(d.Tags) == 0 || len(d.Tags) > 8 {
		return fmt.Errorf("tag count %d is outside 1-8", len(d.Tags))
	}
	return nil
}
