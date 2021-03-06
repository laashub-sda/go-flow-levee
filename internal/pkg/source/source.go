// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package source contains the logic related to the concept of the source which may be tainted.
package source

import (
	"fmt"
	"go/types"
	"strings"

	"github.com/google/go-flow-levee/internal/pkg/sanitizer"
	"github.com/google/go-flow-levee/internal/pkg/utils"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"
)

type classifier interface {
	IsSource(types.Type) bool
	IsSanitizer(*ssa.Call) bool
	IsPropagator(*ssa.Call) bool
	IsSourceFieldAddr(*ssa.FieldAddr) bool
}

// Source represents a Source in an SSA call tree.
// It is based on ssa.Node, with the added functionality of computing the recursive graph of
// its referrers.
// Source.sanitized notes sanitizer calls that sanitize this Source
type Source struct {
	node       ssa.Node
	marked     map[ssa.Node]bool
	preOrder   []ssa.Node
	sanitizers []*sanitizer.Sanitizer
	config     classifier
}

// Node returns the underlying ssa.Node of the Source.
func (a *Source) Node() ssa.Node {
	return a.node
}

// New constructs a Source
func New(in ssa.Node, config classifier) *Source {
	a := &Source{
		node:   in,
		marked: make(map[ssa.Node]bool),
		config: config,
	}
	a.dfs(in)
	return a
}

// dfs performs Depth-First-Search on the def-use graph of the input Source.
// While traversing the graph we also look for potential sanitizers of this Source.
// If the Source passes through a sanitizer, dfs does not continue through that Node.
func (a *Source) dfs(n ssa.Node) {
	a.preOrder = append(a.preOrder, n)
	a.marked[n.(ssa.Node)] = true

	if n.Referrers() == nil {
		return
	}

	for _, r := range *n.Referrers() {
		if a.marked[r.(ssa.Node)] {
			continue
		}

		switch v := r.(type) {
		case *ssa.Call:
			// This is to avoid attaching calls where the source is the receiver, ex:
			// core.Sinkf("Source id: %v", wrapper.Source.GetID())
			if v.Call.Signature().Recv() != nil {
				continue
			}

			if a.config.IsSanitizer(v) {
				a.sanitizers = append(a.sanitizers, &sanitizer.Sanitizer{Call: v})
			}
		case *ssa.FieldAddr:
			if !a.config.IsSourceFieldAddr(v) {
				continue
			}
		}

		a.dfs(r.(ssa.Node))
	}
}

// compress removes the elements from the graph that are not required by the
// taint-propagation analysis. Concretely, only propagators, sanitizers and
// sinks should constitute the output. Since, we already know what the source
// is, it is also removed.
func (a *Source) compress() []ssa.Node {
	var compressed []ssa.Node
	for _, n := range a.preOrder {
		switch n.(type) {
		case *ssa.Call:
			compressed = append(compressed, n)
		}
	}

	return compressed
}

// HasPathTo returns true when a Node is part of declaration-use graph.
func (a *Source) HasPathTo(n ssa.Node) bool {
	return a.marked[n]
}

// IsSanitizedAt returns true when the Source is sanitized by the supplied instruction.
func (a *Source) IsSanitizedAt(call ssa.Instruction) bool {
	for _, s := range a.sanitizers {
		if s.Dominates(call) {
			return true
		}
	}

	return false
}

// String implements Stringer interface.
func (a *Source) String() string {
	var b strings.Builder
	for _, n := range a.compress() {
		b.WriteString(fmt.Sprintf("%v ", n))
	}

	return b.String()
}

func identify(conf classifier, ssaInput *buildssa.SSA) map[*ssa.Function][]*Source {
	sourceMap := make(map[*ssa.Function][]*Source)

	for _, fn := range ssaInput.SrcFuncs {
		var sources []*Source
		sources = append(sources, sourcesFromParams(fn, conf)...)
		sources = append(sources, sourcesFromClosure(fn, conf)...)
		sources = append(sources, sourcesFromBlocks(fn, conf)...)

		if len(sources) > 0 {
			sourceMap[fn] = sources
		}
	}
	return sourceMap
}

func sourcesFromParams(fn *ssa.Function, conf classifier) []*Source {
	var sources []*Source
	for _, p := range fn.Params {
		switch t := p.Type().(type) {
		case *types.Pointer:
			if n, ok := t.Elem().(*types.Named); ok && conf.IsSource(n) {
				sources = append(sources, New(p, conf))
			}
			// TODO Handle the case where sources arepassed by value: func(c sourceType)
			// TODO Handle cases where PII is wrapped in struct/slice/map
		}
	}
	return sources
}

func sourcesFromClosure(fn *ssa.Function, conf classifier) []*Source {
	var sources []*Source
	for _, p := range fn.FreeVars {
		switch t := p.Type().(type) {
		case *types.Pointer:
			// FreeVars (variables from a closure) appear as double-pointers
			// Hence, the need to dereference them recursively.
			if s, ok := utils.Dereference(t).(*types.Named); ok && conf.IsSource(s) {
				sources = append(sources, New(p, conf))
			}
		}
	}
	return sources
}

func sourcesFromBlocks(fn *ssa.Function, conf classifier) []*Source {
	var sources []*Source
	for _, b := range fn.Blocks {
		if b == fn.Recover {
			// TODO Handle calls to log in a recovery block.
			continue
		}

		for _, instr := range b.Instrs {
			switch v := instr.(type) {
			// Looking for sources of PII allocated within the body of a function.
			case *ssa.Alloc:
				if conf.IsSource(utils.Dereference(v.Type())) && !isProducedBySanitizer(v, conf) {
					sources = append(sources, New(v, conf))
				}

				// Handling the case where PII may be in a receiver
				// (ex. func(b *something) { log.Info(something.PII) }
			case *ssa.FieldAddr:
				if conf.IsSource(utils.Dereference(v.Type())) {
					sources = append(sources, New(v, conf))
				}
			}
		}
	}
	return sources
}

func isProducedBySanitizer(v *ssa.Alloc, conf classifier) bool {
	for _, instr := range *v.Referrers() {
		store, ok := instr.(*ssa.Store)
		if !ok {
			continue
		}
		call, ok := store.Val.(*ssa.Call)
		if !ok {
			continue
		}
		if conf.IsSanitizer(call) {
			return true
		}
	}
	return false
}
