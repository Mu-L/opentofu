// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package remote

import (
	"context"
	"fmt"
	"log"
	"strings"

	tfe "github.com/hashicorp/go-tfe"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/backend"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/states/statemgr"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tofu"
)

// Ensure that remote.Remote implements the backend.Local interface.
var _ backend.Local = (*Remote)(nil)

// LocalRun implements backend.Local.
func (b *Remote) LocalRun(ctx context.Context, op *backend.Operation) (*backend.LocalRun, statemgr.Full, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	ret := &backend.LocalRun{
		PlanOpts: &tofu.PlanOpts{
			Mode:     op.PlanMode,
			Targets:  op.Targets,
			Excludes: op.Excludes,
		},
	}

	op.StateLocker = op.StateLocker.WithContext(context.Background())

	// Get the remote workspace name.
	remoteWorkspaceName := b.getRemoteWorkspaceName(op.Workspace)

	// Get the latest state.
	log.Printf("[TRACE] backend/remote: requesting state manager for workspace %q", remoteWorkspaceName)
	stateMgr, err := b.StateMgr(ctx, op.Workspace)
	if err != nil {
		diags = diags.Append(fmt.Errorf("error loading state: %w", err))
		return nil, nil, diags
	}

	log.Printf("[TRACE] backend/remote: requesting state lock for workspace %q", remoteWorkspaceName)
	if diags := op.StateLocker.Lock(stateMgr, op.Type.String()); diags.HasErrors() {
		return nil, nil, diags
	}

	defer func() {
		// If we're returning with errors, and thus not producing a valid
		// context, we'll want to avoid leaving the remote workspace locked.
		if diags.HasErrors() {
			diags = diags.Append(op.StateLocker.Unlock())
		}
	}()

	log.Printf("[TRACE] backend/remote: reading remote state for workspace %q", remoteWorkspaceName)
	if err := stateMgr.RefreshState(context.TODO()); err != nil {
		diags = diags.Append(fmt.Errorf("error loading state: %w", err))
		return nil, nil, diags
	}

	// Initialize our context options
	var opts tofu.ContextOpts
	if v := b.ContextOpts; v != nil {
		opts = *v
	}

	// Copy set options from the operation
	opts.UIInput = op.UIIn
	opts.Encryption = op.Encryption

	// Load the latest state. If we enter contextFromPlanFile below then the
	// state snapshot in the plan file must match this, or else it'll return
	// error diagnostics.
	log.Printf("[TRACE] backend/remote: retrieving remote state snapshot for workspace %q", remoteWorkspaceName)
	ret.InputState = stateMgr.State()

	log.Printf("[TRACE] backend/remote: loading configuration for the current working directory")
	config, configDiags := op.ConfigLoader.LoadConfig(ctx, op.ConfigDir, op.RootCall)
	diags = diags.Append(configDiags)
	if configDiags.HasErrors() {
		return nil, nil, diags
	}
	ret.Config = config

	if op.AllowUnsetVariables {
		// If we're not going to use the variables in an operation we'll be
		// more lax about them, stubbing out any unset ones as unknown.
		// This gives us enough information to produce a consistent context,
		// but not enough information to run a real operation (plan, apply, etc)
		ret.PlanOpts.SetVariables = stubAllVariables(op.Variables, config.Module.Variables)
	} else {
		// The underlying API expects us to use the opaque workspace id to request
		// variables, so we'll need to look that up using our organization name
		// and workspace name.
		remoteWorkspaceID, err := b.getRemoteWorkspaceID(context.Background(), op.Workspace)
		if err != nil {
			diags = diags.Append(fmt.Errorf("error finding remote workspace: %w", err))
			return nil, nil, diags
		}

		w, err := b.fetchWorkspace(context.Background(), b.organization, op.Workspace)
		if err != nil {
			diags = diags.Append(fmt.Errorf("error loading workspace: %w", err))
			return nil, nil, diags
		}

		if isLocalExecutionMode(w.ExecutionMode) {
			log.Printf("[TRACE] skipping retrieving variables from workspace %s/%s (%s), workspace is in Local Execution mode", remoteWorkspaceName, b.organization, remoteWorkspaceID)
		} else {
			log.Printf("[TRACE] backend/remote: retrieving variables from workspace %s/%s (%s)", remoteWorkspaceName, b.organization, remoteWorkspaceID)
			tfeVariables, err := b.client.Variables.List(context.Background(), remoteWorkspaceID, nil)
			if err != nil && err != tfe.ErrResourceNotFound {
				diags = diags.Append(fmt.Errorf("error loading variables: %w", err))
				return nil, nil, diags
			}
			if tfeVariables != nil {
				if op.Variables == nil {
					op.Variables = make(map[string]backend.UnparsedVariableValue)
				}
				for _, v := range tfeVariables.Items {
					if v.Category == tfe.CategoryTerraform {
						if _, ok := op.Variables[v.Key]; !ok {
							op.Variables[v.Key] = &remoteStoredVariableValue{
								definition: v,
							}
						}
					}
				}
			}
		}

		if op.Variables != nil {
			variables, varDiags := backend.ParseVariableValues(op.Variables, config.Module.Variables)
			diags = diags.Append(varDiags)
			if diags.HasErrors() {
				return nil, nil, diags
			}
			ret.PlanOpts.SetVariables = variables
		}
	}

	tfCtx, ctxDiags := tofu.NewContext(&opts)
	diags = diags.Append(ctxDiags)
	ret.Core = tfCtx

	log.Printf("[TRACE] backend/remote: finished building tofu.Context")

	return ret, stateMgr, diags
}

func (b *Remote) getRemoteWorkspaceName(localWorkspaceName string) string {
	switch {
	case localWorkspaceName == backend.DefaultStateName:
		// The default workspace name is a special case, for when the backend
		// is configured to with to an exact remote workspace rather than with
		// a remote workspace _prefix_.
		return b.workspace
	case b.prefix != "" && !strings.HasPrefix(localWorkspaceName, b.prefix):
		return b.prefix + localWorkspaceName
	default:
		return localWorkspaceName
	}
}

func (b *Remote) getRemoteWorkspace(ctx context.Context, localWorkspaceName string) (*tfe.Workspace, error) {
	remoteWorkspaceName := b.getRemoteWorkspaceName(localWorkspaceName)

	log.Printf("[TRACE] backend/remote: looking up workspace for %s/%s", b.organization, remoteWorkspaceName)
	remoteWorkspace, err := b.client.Workspaces.Read(ctx, b.organization, remoteWorkspaceName)
	if err != nil {
		return nil, err
	}

	return remoteWorkspace, nil
}

func (b *Remote) getRemoteWorkspaceID(ctx context.Context, localWorkspaceName string) (string, error) {
	remoteWorkspace, err := b.getRemoteWorkspace(ctx, localWorkspaceName)
	if err != nil {
		return "", err
	}

	return remoteWorkspace.ID, nil
}

func stubAllVariables(vv map[string]backend.UnparsedVariableValue, decls map[string]*configs.Variable) tofu.InputValues {
	ret := make(tofu.InputValues, len(decls))

	for name, cfg := range decls {
		raw, exists := vv[name]
		if !exists {
			ret[name] = &tofu.InputValue{
				Value:      cty.UnknownVal(cfg.Type),
				SourceType: tofu.ValueFromConfig,
			}
			continue
		}

		val, diags := raw.ParseVariableValue(cfg.ParsingMode)
		if diags.HasErrors() {
			ret[name] = &tofu.InputValue{
				Value:      cty.UnknownVal(cfg.Type),
				SourceType: tofu.ValueFromConfig,
			}
			continue
		}
		ret[name] = val
	}

	return ret
}

// remoteStoredVariableValue is a backend.UnparsedVariableValue implementation
// that translates from the go-tfe representation of stored variables into
// the OpenTofu Core backend representation of variables.
type remoteStoredVariableValue struct {
	definition *tfe.Variable
}

var _ backend.UnparsedVariableValue = (*remoteStoredVariableValue)(nil)

func (v *remoteStoredVariableValue) ParseVariableValue(mode configs.VariableParsingMode) (*tofu.InputValue, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	var val cty.Value

	switch {
	case v.definition.Sensitive:
		// If it's marked as sensitive then it's not available for use in
		// local operations. We'll use an unknown value as a placeholder for
		// it so that operations that don't need it might still work, but
		// we'll also produce a warning about it to add context for any
		// errors that might result here.
		val = cty.DynamicVal
		if !v.definition.HCL {
			// If it's not marked as HCL then we at least know that the
			// value must be a string, so we'll set that in case it allows
			// us to do some more precise type checking.
			val = cty.UnknownVal(cty.String)
		}

		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Warning,
			fmt.Sprintf("Value for var.%s unavailable", v.definition.Key),
			fmt.Sprintf("The value of variable %q is marked as sensitive in the remote workspace. This operation always runs locally, so the value for that variable is not available.", v.definition.Key),
		))

	case v.definition.HCL:
		// If the variable value is marked as being in HCL syntax, we need to
		// parse it the same way as it would be interpreted in a .tfvars
		// file because that is how it would get passed to OpenTofu CLI for
		// a remote operation and we want to mimic that result as closely as
		// possible.
		var exprDiags hcl.Diagnostics
		expr, exprDiags := hclsyntax.ParseExpression([]byte(v.definition.Value), "<remote workspace>", hcl.Pos{Line: 1, Column: 1})
		if expr != nil {
			var moreDiags hcl.Diagnostics
			val, moreDiags = expr.Value(nil)
			exprDiags = append(exprDiags, moreDiags...)
		} else {
			// We'll have already put some errors in exprDiags above, so we'll
			// just stub out the value here.
			val = cty.DynamicVal
		}

		// We don't have sufficient context to return decent error messages
		// for syntax errors in the remote values, so we'll just return a
		// generic message instead for now.
		// (More complete error messages will still result from true remote
		// operations, because they'll run on the remote system where we've
		// materialized the values into a tfvars file we can report from.)
		if exprDiags.HasErrors() {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				fmt.Sprintf("Invalid expression for var.%s", v.definition.Key),
				fmt.Sprintf("The value of variable %q is marked in the remote workspace as being specified in HCL syntax, but the given value is not valid HCL. Stored variable values must be valid literal expressions and may not contain references to other variables or calls to functions.", v.definition.Key),
			))
		}

	default:
		// A variable value _not_ marked as HCL is always be a string, given
		// literally.
		val = cty.StringVal(v.definition.Value)
	}

	return &tofu.InputValue{
		Value: val,

		// We mark these as "from input" with the rationale that entering
		// variable values into the Terraform Cloud or Enterprise UI is,
		// roughly speaking, a similar idea to entering variable values at
		// the interactive CLI prompts. It's not a perfect correspondence,
		// but it's closer than the other options.
		SourceType: tofu.ValueFromInput,
	}, diags
}
