// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc

import "cmd/compile/internal/ir"

// Strongly connected components.
//
// Run analysis on minimal sets of mutually recursive functions
// or single non-recursive functions, bottom up.
//
// Finding these sets is finding strongly connected components
// by reverse topological order in the static call graph.
// The algorithm (known as Tarjan's algorithm) for doing that is taken from
// Sedgewick, Algorithms, Second Edition, p. 482, with two adaptations.
//
// First, a hidden closure function (n.Func.IsHiddenClosure()) cannot be the
// root of a connected component. Refusing to use it as a root
// forces it into the component of the function in which it appears.
// This is more convenient for escape analysis.
//
// Second, each function becomes two virtual nodes in the graph,
// with numbers n and n+1. We record the function's node number as n
// but search from node n+1. If the search tells us that the component
// number (min) is n+1, we know that this is a trivial component: one function
// plus its closures. If the search tells us that the component number is
// n, then there was a path from node n+1 back to node n, meaning that
// the function set is mutually recursive. The escape analysis can be
// more precise when analyzing a single non-recursive function than
// when analyzing a set of mutually recursive functions.

type bottomUpVisitor struct {
	analyze  func([]*ir.Func, bool)
	visitgen uint32
	nodeID   map[*ir.Func]uint32
	stack    []*ir.Func
}

// visitBottomUp invokes analyze on the ODCLFUNC nodes listed in list.
// It calls analyze with successive groups of functions, working from
// the bottom of the call graph upward. Each time analyze is called with
// a list of functions, every function on that list only calls other functions
// on the list or functions that have been passed in previous invocations of
// analyze. Closures appear in the same list as their outer functions.
// The lists are as short as possible while preserving those requirements.
// (In a typical program, many invocations of analyze will be passed just
// a single function.) The boolean argument 'recursive' passed to analyze
// specifies whether the functions on the list are mutually recursive.
// If recursive is false, the list consists of only a single function and its closures.
// If recursive is true, the list may still contain only a single function,
// if that function is itself recursive.
func visitBottomUp(list []ir.Node, analyze func(list []*ir.Func, recursive bool)) {
	var v bottomUpVisitor
	v.analyze = analyze
	v.nodeID = make(map[*ir.Func]uint32)
	for _, n := range list {
		if n.Op() == ir.ODCLFUNC {
			n := n.(*ir.Func)
			if !n.Func().IsHiddenClosure() {
				v.visit(n)
			}
		}
	}
}

func (v *bottomUpVisitor) visit(n *ir.Func) uint32 {
	if id := v.nodeID[n]; id > 0 {
		// already visited
		return id
	}

	v.visitgen++
	id := v.visitgen
	v.nodeID[n] = id
	v.visitgen++
	min := v.visitgen
	v.stack = append(v.stack, n)

	ir.Visit(n, func(n ir.Node) {
		switch n.Op() {
		case ir.ONAME:
			if n.Class() == ir.PFUNC {
				if n != nil && n.Name().Defn != nil {
					if m := v.visit(n.Name().Defn.(*ir.Func)); m < min {
						min = m
					}
				}
			}
		case ir.OMETHEXPR:
			fn := methodExprName(n)
			if fn != nil && fn.Defn != nil {
				if m := v.visit(fn.Defn.(*ir.Func)); m < min {
					min = m
				}
			}
		case ir.ODOTMETH:
			fn := methodExprName(n)
			if fn != nil && fn.Op() == ir.ONAME && fn.Class() == ir.PFUNC && fn.Defn != nil {
				if m := v.visit(fn.Defn.(*ir.Func)); m < min {
					min = m
				}
			}
		case ir.OCALLPART:
			fn := ir.AsNode(callpartMethod(n).Nname)
			if fn != nil && fn.Op() == ir.ONAME {
				if fn := fn.(*ir.Name); fn.Class() == ir.PFUNC && fn.Name().Defn != nil {
					if m := v.visit(fn.Name().Defn.(*ir.Func)); m < min {
						min = m
					}
				}
			}
		case ir.OCLOSURE:
			n := n.(*ir.ClosureExpr)
			if m := v.visit(n.Func()); m < min {
				min = m
			}
		}
	})

	if (min == id || min == id+1) && !n.IsHiddenClosure() {
		// This node is the root of a strongly connected component.

		// The original min passed to visitcodelist was v.nodeID[n]+1.
		// If visitcodelist found its way back to v.nodeID[n], then this
		// block is a set of mutually recursive functions.
		// Otherwise it's just a lone function that does not recurse.
		recursive := min == id

		// Remove connected component from stack.
		// Mark walkgen so that future visits return a large number
		// so as not to affect the caller's min.

		var i int
		for i = len(v.stack) - 1; i >= 0; i-- {
			x := v.stack[i]
			if x == n {
				break
			}
			v.nodeID[x] = ^uint32(0)
		}
		v.nodeID[n] = ^uint32(0)
		block := v.stack[i:]
		// Run escape analysis on this set of functions.
		v.stack = v.stack[:i]
		v.analyze(block, recursive)
	}

	return min
}
