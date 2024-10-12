// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package addrs

import (
	"fmt"
	"strings"

	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// ProviderInstance is an interface type whose dynamic type can be either
// LocalProviderInstance or AbsProviderInstance, in order to represent
// situations where a value might either be module-local or absolute but the
// decision cannot be made until runtime.
//
// Where possible, use either LocalProviderInstance or ConfigProviderInstance
// directly instead, to make intent more clear. ProviderInstance can be used only
// in situations where the recipient of the value has some out-of-band way to
// determine a "current module" to use if the value turns out to be
// a LocalProviderInstance.
//
// Recipients of non-nil ProviderInstance values that actually need
// ConfigProviderInstance values should call ResolveAbsProviderAddr on the
// *configs.Config value representing the root module configuration, which
// handles the translation from local to fully-qualified using mapping tables
// defined in the configuration.
//
// Recipients of a ProviderInstance value can assume it can contain only a
// LocalProviderInstance value, an ConfigProviderInstance value, or nil to
// represent the absence of a provider config in situations where that is
// meaningful.
type ProviderInstance interface {
	providerInstance()
}

// LocalProviderInstance is the address of a provider configuration from the
// perspective of references in a particular module.
//
// Finding the corresponding ConfigProviderInstance will require looking up the
// LocalName in the providers table in the module's configuration; there is
// no syntax-only translation between these types.
type LocalProviderInstance struct {
	LocalName string
	Key       InstanceKey
}

var _ ProviderInstance = LocalProviderInstance{}
var _ Referenceable = LocalProviderInstance{}

// NewDefaultLocalProviderInstance returns the address of the default (un-aliased)
// configuration for the provider with the given local type name.
func NewDefaultLocalProviderInstance(LocalNameName string) LocalProviderInstance {
	return LocalProviderInstance{
		LocalName: LocalNameName,
	}
}

// providerInstance Implements addrs.ProviderInstance.
func (pc LocalProviderInstance) providerInstance() {}

func (pc LocalProviderInstance) String() string {
	if pc.LocalName == "" {
		// Should never happen; always indicates a bug
		return "provider.<invalid>"
	}

	if pc.Key != NoKey {
		if strKey, ok := pc.Key.(StringKey); ok && hclsyntax.ValidIdentifier(string(strKey)) {
			// We'll return using the old-style identifier syntax if possible,
			// since that's backward-compatible.
			return fmt.Sprintf("provider.%s.%s", pc.LocalName, strKey)
		}
		// Otherwise we'll use the more general index syntax, so that
		// we can evolve towards treating providers more like everything else.
		return fmt.Sprintf("provider.%s%s", pc.LocalName, pc.Key)
	}

	return "provider." + pc.LocalName
}

// StringCompact is an alternative to String that returns the compact form
// without the "provider." prefix.
func (pc LocalProviderInstance) StringCompact() string {
	if pc.Key != NoKey {
		if strKey, ok := pc.Key.(StringKey); ok && hclsyntax.ValidIdentifier(string(strKey)) {
			// We'll return using the old-style identifier syntax if possible,
			// since that's backward-compatible.
			return fmt.Sprintf("%s.%s", pc.LocalName, strKey)
		}
		// Otherwise we'll use the more general index syntax, so that
		// we can evolve towards treating providers more like everything else.
		return fmt.Sprintf("%s%s", pc.LocalName, pc.Key)
	}
	return pc.LocalName
}

// UniqueKey implements UniqueKeyer and Referenceable.
func (pc LocalProviderInstance) UniqueKey() UniqueKey {
	// A LocalProviderInstance can be its own UniqueKey
	return pc
}

// uniqueKeySigil implements UniqueKey.
func (pc LocalProviderInstance) uniqueKeySigil() {}

// referenceableSigil implements Referenceable.
func (pc LocalProviderInstance) referenceableSigil() {}

// AbsProviderInstance represents the fully-qualified of an instance of a
// provider, after instance expansion is complete.
//
// Each "provider" block in the configuration can become zero or more
// AbsProviderInstance after expansion, and all of the expanded instances
// must have unique AbsProviderInstance addresses.
type AbsProviderInstance struct {
	Module   ModuleInstance
	Provider Provider

	// Key is the instance key of an additional (aka "aliased") provider
	// instance. This is populated either from the "alias" argument of
	// the associated provider configuration, or from one of the keys
	// in the for_each argument.
	//
	// Unlike most other multi-instance address types, the key for a
	// provider instance is currently always either NoKey or a string,
	// and a string key always contains a valid HCL identifier. However,
	// best to try to avoid depending on those constraints as much as
	// possible in other code and in new language features so that we
	// can potentially generalize this more later if someone finds a
	// compelling use-case for making this behave more like other
	// expandable objects.
	Key InstanceKey
}

var _ ProviderInstance = AbsProviderInstance{}
var _ UniqueKeyer = AbsProviderInstance{}

// ParseConfigProviderInstance parses the given traversal as an absolute
// provider instance address. The following are examples of traversals that can be
// successfully parsed as provider instance addresses using this function:
//
//   - provider["registry.opentofu.org/hashicorp/aws"]
//   - provider["registry.opentofu.org/hashicorp/aws"].foo
//   - module.bar.provider["registry.opentofu.org/hashicorp/aws"]
//   - module.bar["foo"].module.baz.provider["registry.opentofu.org/hashicorp/aws"].foo
//
// This type of address is used, for example, to record the relationships
// between resources and provider configurations in the state structure.
// This type of address is typically not used prominently in the UI, except in
// error messages that refer to provider configurations.
func ParseAbsProviderInstance(traversal hcl.Traversal) (AbsProviderInstance, tfdiags.Diagnostics) {
	modInst, remain, diags := parseModuleInstancePrefix(traversal)
	var ret AbsProviderInstance

	ret.Module = modInst

	if len(remain) < 2 || remain.RootName() != "provider" {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid provider configuration address",
			Detail:   "Provider address must begin with \"provider.\", followed by a provider type name.",
			Subject:  remain.SourceRange().Ptr(),
		})
		return ret, diags
	}
	if len(remain) > 3 {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid provider configuration address",
			Detail:   "Extraneous operators after provider configuration alias.",
			Subject:  hcl.Traversal(remain[3:]).SourceRange().Ptr(),
		})
		return ret, diags
	}

	if tt, ok := remain[1].(hcl.TraverseIndex); ok {
		if !tt.Key.Type().Equals(cty.String) {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid provider configuration address",
				Detail:   "The prefix \"provider.\" must be followed by a provider type name.",
				Subject:  remain[1].SourceRange().Ptr(),
			})
			return ret, diags
		}
		p, sourceDiags := ParseProviderSourceString(tt.Key.AsString())
		ret.Provider = p
		if sourceDiags.HasErrors() {
			diags = diags.Append(sourceDiags)
			return ret, diags
		}
	} else {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid provider configuration address",
			Detail:   "The prefix \"provider.\" must be followed by a provider type name.",
			Subject:  remain[1].SourceRange().Ptr(),
		})
		return ret, diags
	}

	if len(remain) == 3 {
		// We accept both attribute and index syntax for this last part, because
		// the attribute syntax is backward-compatible with older versions that
		// think this is just a static alias identifier, while index is more
		// general as we try to make progress towards provider instances being
		// increasingly similar to everything else that can dynamic-expand
		// over time.
		switch tt := remain[2].(type) {
		case hcl.TraverseAttr:
			ret.Key = StringKey(tt.Name)
		case hcl.TraverseIndex:
			switch tt.Key.Type() {
			case cty.String:
				ret.Key = StringKey(tt.Key.AsString())
			default:
				// No other types are allowed for provider instance keys in particular.
				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Invalid address operator",
					Detail:   "Invalid provider instance key: must be a string.",
					Subject:  tt.SourceRange().Ptr(),
				})
				return ret, diags
			}
		default:
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid provider configuration address",
				Detail:   "Provider type name must be followed by a configuration alias name.",
				Subject:  remain[2].SourceRange().Ptr(),
			})
			return ret, diags
		}
	}

	return ret, diags
}

// ParseAbsProviderInstanceStr is a helper wrapper around ParseAbsProviderInstance
// that takes a string and parses it with the HCL native syntax traversal parser
// before interpreting it.
//
// This should be used only in specialized situations since it will cause the
// created references to not have any meaningful source location information.
// If a reference string is coming from a source that should be identified in
// error messages then the caller should instead parse it directly using a
// suitable function from the HCL API and pass the traversal itself to
// ParseAbsProviderInstance.
//
// Error diagnostics are returned if either the parsing fails or the analysis
// of the traversal fails. There is no way for the caller to distinguish the
// two kinds of diagnostics programmatically. If error diagnostics are returned
// the returned address is invalid.
func ParseAbsProviderInstanceStr(str string) (AbsProviderInstance, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	traversal, parseDiags := hclsyntax.ParseTraversalAbs([]byte(str), "", hcl.Pos{Line: 1, Column: 1})
	diags = diags.Append(parseDiags)
	if parseDiags.HasErrors() {
		return AbsProviderInstance{}, diags
	}
	addr, addrDiags := ParseAbsProviderInstance(traversal)
	diags = diags.Append(addrDiags)
	return addr, diags
}

// String returns a string representation of the instance address suitable for
// display in the UI when talking about providers in global scope.
//
// This representation isn't so appropriate for situations when talking about
// provider instances only within a specific module. In that case it might be
// better to translate to a [LocalProviderInstance] and use its string
// representation so that the provider is described using the module's
// chosen short local name, rather than the global provider source address.
func (pi AbsProviderInstance) String() string {
	var buf strings.Builder
	if !pi.Module.IsRoot() {
		buf.WriteString(pi.Module.String())
		buf.WriteByte('.')
	}
	fmt.Fprintf(&buf, "provider[%s]", pi.Provider)
	if pi.Key != nil {
		if str, ok := pi.Key.(StringKey); ok && hclsyntax.ValidIdentifier(string(str)) {
			// We prefer to use the attribute syntax if the key is valid for it,
			// because that's backward-compatible with older versions of OpenTofu
			// that think this portion just represents a static alias identifier.
			buf.WriteByte('.')
			buf.WriteString(string(str))
		} else {
			buf.WriteString(pi.Key.String())
		}
	}
	return buf.String()
}

// ParseLegacyAbsProviderInstance parses the given traversal as an absolute
// provider address in the legacy form used by OpenTofu v0.12 and earlier.
// The following are examples of traversals that can be successfully parsed as
// legacy absolute provider configuration addresses:
//
//   - provider.aws
//   - provider.aws.foo
//   - module.bar.provider.aws
//   - module.bar.module.baz.provider.aws.foo
//
// We can encounter this kind of address in a historical state snapshot that
// hasn't yet been upgraded by refreshing or applying a plan with
// OpenTofu v0.13. Later versions of OpenTofu reject state snapshots using
// this format, and so users must follow the OpenTofu v0.13 upgrade guide
// in that case.
//
// We will not use this address form for any new file formats.
func ParseLegacyAbsProviderInstance(traversal hcl.Traversal) (AbsProviderInstance, tfdiags.Diagnostics) {
	modInst, remain, diags := parseModuleInstancePrefix(traversal)
	var ret AbsProviderInstance

	// OpenTofu v0.12 and earlier didn't have dynamic module instances yet,
	// so if we encounter those then this can't possibly be a legacy address.
	for _, step := range modInst {
		if step.InstanceKey != NoKey {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid provider configuration address",
				Detail:   "Legacy provider instance address cannot contain module instance key",
				Subject:  remain.SourceRange().Ptr(),
			})
			return ret, diags
		}
	}
	ret.Module = modInst

	if len(remain) < 2 || remain.RootName() != "provider" {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid provider configuration address",
			Detail:   "Provider address must begin with \"provider.\", followed by a provider type name.",
			Subject:  remain.SourceRange().Ptr(),
		})
		return ret, diags
	}
	if len(remain) > 3 {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid provider configuration address",
			Detail:   "Extraneous operators after provider configuration alias.",
			Subject:  hcl.Traversal(remain[3:]).SourceRange().Ptr(),
		})
		return ret, diags
	}

	// We always assume legacy-style providers in legacy state ...
	if tt, ok := remain[1].(hcl.TraverseAttr); ok {
		// ... unless it's the builtin "terraform" provider, a special case.
		if tt.Name == "terraform" {
			ret.Provider = NewBuiltInProvider(tt.Name)
		} else {
			ret.Provider = NewLegacyProvider(tt.Name)
		}
	} else {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid provider configuration address",
			Detail:   "The prefix \"provider.\" must be followed by a provider type name.",
			Subject:  remain[1].SourceRange().Ptr(),
		})
		return ret, diags
	}

	if len(remain) == 3 {
		if tt, ok := remain[2].(hcl.TraverseAttr); ok {
			ret.Key = StringKey(tt.Name)
		} else {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid provider configuration address",
				Detail:   "Provider type name must be followed by a configuration alias name.",
				Subject:  remain[2].SourceRange().Ptr(),
			})
			return ret, diags
		}
	}

	return ret, diags
}

func ParseLegacyAbsProviderInstanceStr(str string) (AbsProviderInstance, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	traversal, parseDiags := hclsyntax.ParseTraversalAbs([]byte(str), "", hcl.Pos{Line: 1, Column: 1})
	diags = diags.Append(parseDiags)
	if parseDiags.HasErrors() {
		return AbsProviderInstance{}, diags
	}

	addr, addrDiags := ParseLegacyAbsProviderInstance(traversal)
	diags = diags.Append(addrDiags)
	return addr, diags
}

// UniqueKey implements UniqueKeyer.
func (pi AbsProviderInstance) UniqueKey() UniqueKey {
	return absProviderInstanceKey{pi.String()}
}

type absProviderInstanceKey struct {
	s string
}

// uniqueKeySigil implements UniqueKey.
func (a absProviderInstanceKey) uniqueKeySigil() {}

// providerInstance implements ProviderInstance.
func (pi AbsProviderInstance) providerInstance() {}
