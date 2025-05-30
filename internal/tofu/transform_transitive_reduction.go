// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tofu

import "context"

// TransitiveReductionTransformer is a GraphTransformer that
// finds the transitive reduction of the graph. For a definition of
// transitive reduction, see [Wikipedia](https://en.wikipedia.org/wiki/Transitive_reduction).
type TransitiveReductionTransformer struct{}

func (t *TransitiveReductionTransformer) Transform(_ context.Context, g *Graph) error {
	// If the graph isn't valid, skip the transitive reduction.
	// We don't error here because OpenTofu itself handles graph
	// validation in a better way, or we assume it does.
	if err := g.Validate(); err != nil {
		return nil
	}

	// Do it
	g.TransitiveReduction()

	return nil
}
