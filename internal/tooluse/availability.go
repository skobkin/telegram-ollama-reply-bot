package tooluse

import "slices"

func (r *Runtime) callContext(req ChatRequest, run *runState) CallContext {
	return CallContext{Scope: req.Scope, Requester: req.RequestMessage,
		RequesterMediaGroupID: req.RequesterMediaGroupID, run: run}
}

func resolveDefinition(d Definition, call CallContext) (Definition, bool) {
	d.Parameters = slices.Clone(d.Parameters)
	if d.Resolve != nil {
		presentation, available := d.Resolve(call, Presentation{d.Description, d.Parameters})
		if !available {
			return d, false
		}
		d.Description, d.Parameters = presentation.Description, slices.Clone(presentation.Parameters)
	}

	return d, true
}

func (r *Runtime) resolveDefinitions(req ChatRequest, run *runState) []Definition {
	definitions := allowedDefinitions(r.turnDefinitions(run.systemOneModel), req.AllowedTools)
	resolved := make([]Definition, 0, len(definitions))
	for _, d := range definitions {
		definition, available := resolveDefinition(d, r.callContext(req, run))
		if !available {
			continue
		}
		if definition.Name == "set_reaction" && req.AllowedTools != nil && !slices.Contains(req.AllowedTools, "say_nothing") {
			definition.Description = setReactionDescription(true)
		}
		resolved = append(resolved, definition)
	}

	return resolved
}
