package planner

import (
	"encoding/json"

	"github.com/kalanas210/runmesh/internal/tools"
)

// ResponseSchema builds the JSON schema the model's output is CONSTRAINED to,
// from the tool catalogue this deployment actually offers.
//
// The schema is generated rather than written out, and the reason is the one
// line in the middle: `"enum": [...tool names...]`. A hand-written schema would
// have `"tool": {"type": "STRING"}`, and the model would occasionally invent
// `csv_parse` because it sounds like something that should exist. With the enum,
// constrained decoding makes an unregistered tool name unrepresentable — the
// decoder cannot emit one. Validation still rejects unknown tools afterwards,
// because "the API guarantees it" is a claim about somebody else's system, but
// the failure mode stops being routine.
//
// Two properties of Gemini's schema dialect shape the rest of this. It is an
// OpenAPI 3.0 SUBSET: there is no `additionalProperties`, so "no other fields"
// cannot be expressed and is enforced by the Go decoder instead; and there is no
// free-form object type, which is why per-tool parameters travel as a STRING
// carrying JSON. `propertyOrdering` is a Gemini extension that fixes the order
// fields are generated in — worth setting because a model that emits
// `depends_on` before it has committed to an `id` produces worse graphs.
func ResponseSchema(toolNames []string, maxSteps int) json.RawMessage {
	if maxSteps < 1 {
		maxSteps = 1
	}
	s := schema{
		Type: "OBJECT",
		Properties: map[string]*schema{
			"name": {
				Type:        "STRING",
				Description: "A short human-readable name for this plan, under 60 characters.",
			},
			"reasoning": {
				Type: "STRING",
				Description: "One or two sentences on why the plan is shaped this way. " +
					"This is recorded for a human reviewer and is never executed.",
			},
			"steps": {
				Type:     "ARRAY",
				MinItems: 1,
				MaxItems: maxSteps,
				Items: &schema{
					Type: "OBJECT",
					Properties: map[string]*schema{
						"id": {
							Type: "STRING",
							Description: "Lower-case identifier, letters digits hyphen underscore, " +
								"unique within the plan. Other steps refer to it by this value.",
						},
						"tool": {
							Type: "STRING",
							// The line this whole function exists for.
							Enum:        toolNames,
							Description: "Which tool runs this step. Must be one of the listed values.",
						},
						"params_json": {
							Type: "STRING",
							Description: "The step's parameters, as a JSON object encoded in a string, " +
								"conforming to that tool's input_schema. Use \"{}\" if the tool needs none. " +
								"It must be a string containing JSON, not a JSON object.",
						},
						"depends_on": {
							Type:  "ARRAY",
							Items: &schema{Type: "STRING"},
							Description: "Ids of steps that must succeed first. Their results are " +
								"given to this step, keyed by step id. Leave empty for a step that " +
								"depends on nothing; steps with no dependency between them run " +
								"concurrently, so do not chain steps to order them unless one " +
								"genuinely needs the other's output.",
						},
						"timeout_seconds": {
							Type:        "INTEGER",
							Description: "How long this single step may run. Omit to accept the default.",
						},
					},
					Required:         []string{"id", "tool", "params_json"},
					PropertyOrdering: []string{"id", "tool", "params_json", "depends_on", "timeout_seconds"},
				},
			},
		},
		Required:         []string{"name", "steps"},
		PropertyOrdering: []string{"name", "reasoning", "steps"},
	}

	// Marshalling cannot fail for this closed, self-built structure; a schema
	// that somehow failed to encode would produce an unconstrained request,
	// which is worse than none, so an empty result is returned instead and the
	// caller sends no schema at all rather than a broken one.
	out, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return out
}

// schema is the subset of OpenAPI 3.0 that Gemini's responseSchema accepts.
// Written out rather than pulled from a library, because it is nine fields and
// a library would be a dependency whose whole job is these nine fields.
type schema struct {
	Type             string             `json:"type"`
	Description      string             `json:"description,omitempty"`
	Enum             []string           `json:"enum,omitempty"`
	Items            *schema            `json:"items,omitempty"`
	Properties       map[string]*schema `json:"properties,omitempty"`
	Required         []string           `json:"required,omitempty"`
	MinItems         int                `json:"minItems,omitempty"`
	MaxItems         int                `json:"maxItems,omitempty"`
	PropertyOrdering []string           `json:"propertyOrdering,omitempty"`
}

// FunctionDeclarations renders the catalogue in Gemini's function-calling
// shape: name, description, and the tool's own input schema.
//
// It is exported and used to build the prompt rather than sent as a `tools`
// block, and that is the interesting decision. Function calling asks the model
// to invoke ONE tool and hand control back; planning needs a whole DAG in one
// response, with dependencies between steps that do not exist yet. Feeding the
// same declarations in as documentation gets the benefit — a precise, schema-level
// description of each tool — without the turn-by-turn protocol, and keeps the
// output one validated document instead of a conversation to reconstruct.
func FunctionDeclarations(ds []tools.Descriptor) json.RawMessage {
	type decl struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	out := make([]decl, 0, len(ds))
	for _, d := range ds {
		params := d.InputSchema
		if len(params) == 0 || !json.Valid(params) {
			params = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, decl{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  params,
		})
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil
	}
	return b
}
