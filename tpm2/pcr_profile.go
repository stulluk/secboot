// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2019 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package tpm2

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/canonical/go-tpm2"

	"golang.org/x/xerrors"
)

// pcrValuesList is a list of PCR value combinations computed from PCRProtectionProfile.
type pcrValuesList []tpm2.PCRValues

// setValue sets the specified PCR to the supplied value for all branches.
func (l pcrValuesList) setValue(alg tpm2.HashAlgorithmId, pcr int, value tpm2.Digest) {
	for _, v := range l {
		v.SetValue(alg, pcr, value)
	}
}

// extendValue extends the specified PCR with the supplied value for all branches.
func (l pcrValuesList) extendValue(alg tpm2.HashAlgorithmId, pcr int, value tpm2.Digest) {
	for _, v := range l {
		if _, ok := v[alg]; !ok {
			v[alg] = make(map[int]tpm2.Digest)
		}
		if _, ok := v[alg][pcr]; !ok {
			v[alg][pcr] = make(tpm2.Digest, alg.Size())
		}
		h := alg.NewHash()
		h.Write(v[alg][pcr])
		h.Write(value)
		v[alg][pcr] = h.Sum(nil)
	}
}

func (l pcrValuesList) copy() (out pcrValuesList) {
	for _, v := range l {
		ov := make(tpm2.PCRValues)
		for alg := range v {
			ov[alg] = make(map[int]tpm2.Digest)
			for pcr := range v[alg] {
				ov[alg][pcr] = v[alg][pcr]
			}
		}
		out = append(out, ov)
	}
	return
}

type pcrProtectionProfileAddPCRValueInstr struct {
	alg   tpm2.HashAlgorithmId
	pcr   int
	value tpm2.Digest
}

type pcrProtectionProfileAddPCRValueFromTPMInstr struct {
	alg tpm2.HashAlgorithmId
	pcr int
}

type pcrProtectionProfileExtendPCRInstr struct {
	alg   tpm2.HashAlgorithmId
	pcr   int
	value tpm2.Digest
}

type pcrProtectionProfileAddProfileORInstr struct {
	profiles []*PCRProtectionProfile
}

// pcrProtectionProfileEndProfileInstr is a pseudo instruction to mark the end of a branch.
type pcrProtectionProfileEndProfileInstr struct{}

// pcrProtectionProfileInstr is a building block of PCRProtectionProfile.
type pcrProtectionProfileInstr interface{}

// PCRProtectionProfile defines the PCR profile used to protect a key sealed with SealKeyToTPM. It contains a sequence of instructions
// for computing combinations of PCR values that a key will be protected against. The profile is built using the methods of this type.
type PCRProtectionProfile struct {
	instrs []pcrProtectionProfileInstr
}

func NewPCRProtectionProfile() *PCRProtectionProfile {
	return &PCRProtectionProfile{}
}

// AddPCRValue adds the supplied value to this profile for the specified PCR. This action replaces any value set previously in this
// profile. The function returns the same PCRProtectionProfile so that calls may be chained.
func (p *PCRProtectionProfile) AddPCRValue(alg tpm2.HashAlgorithmId, pcr int, value tpm2.Digest) *PCRProtectionProfile {
	if len(value) != alg.Size() {
		panic("invalid digest length")
	}
	p.instrs = append(p.instrs, &pcrProtectionProfileAddPCRValueInstr{alg: alg, pcr: pcr, value: value})
	return p
}

// AddPCRValueFromTPM adds the current value of the specified PCR to this profile. This action replaces any value set previously in
// this profile. The current value is read back from the TPM when the PCR values generated by this profile are computed. The function
// returns the same PCRProtectionProfile so that calls may be chained.
func (p *PCRProtectionProfile) AddPCRValueFromTPM(alg tpm2.HashAlgorithmId, pcr int) *PCRProtectionProfile {
	p.instrs = append(p.instrs, &pcrProtectionProfileAddPCRValueFromTPMInstr{alg: alg, pcr: pcr})
	return p
}

// ExtendPCR extends the value of the specified PCR in this profile with the supplied value. If this profile doesn't yet have a
// value for the specified PCR, an initial value of all zeroes will be added first. The function returns the same PCRProtectionProfile
// so that calls may be chained.
func (p *PCRProtectionProfile) ExtendPCR(alg tpm2.HashAlgorithmId, pcr int, value tpm2.Digest) *PCRProtectionProfile {
	if len(value) != alg.Size() {
		panic("invalid digest length")
	}
	p.instrs = append(p.instrs, &pcrProtectionProfileExtendPCRInstr{alg: alg, pcr: pcr, value: value})
	return p
}

// AddProfileOR adds one or more sub-profiles that can be used to define PCR policies for multiple conditions. Note that each
// branch must explicitly define values for the same set of PCRs. It is not possible to generate policies where each branch
// defines values for a different set of PCRs. When computing the PCR values for this profile, the sub-profiles added by this command
// will inherit the PCR values computed by this profile. The function returns the same PCRProtectionProfile so that calls may be
// chained.
func (p *PCRProtectionProfile) AddProfileOR(profiles ...*PCRProtectionProfile) *PCRProtectionProfile {
	p.instrs = append(p.instrs, &pcrProtectionProfileAddProfileORInstr{profiles: profiles})
	return p
}

// pcrProtectionProfileIterator provides a mechanism to perform a depth first traversal of instructions in a PCRProtectionProfile.
type pcrProtectionProfileIterator struct {
	instrs [][]pcrProtectionProfileInstr
}

// descendInToProfiles adds instructions from the supplied profiles to the front of the iterator, so that subsequent calls to
// next will return instructions from each of these profiles in turn.
func (iter *pcrProtectionProfileIterator) descendInToProfiles(profiles ...*PCRProtectionProfile) {
	instrs := make([][]pcrProtectionProfileInstr, 0, len(profiles)+len(iter.instrs))
	for _, p := range profiles {
		instrs = append(instrs, p.instrs)
	}
	instrs = append(instrs, iter.instrs...)
	iter.instrs = instrs
}

// next returns the next instruction from this iterator. When encountering a branch, a *pcrProtectionProfileAddProfileORInstr will
// be returned, which indicates the number of alternate branches. Subsequent calls to next will return instructions from each of
// these sub-branches in turn, with each branch terminating with *pcrProtectionProfileEndProfileInstr. Once all sub-branches have
// been processed, subsequent calls to next will resume returning instructions from the parent branch.
func (iter *pcrProtectionProfileIterator) next() pcrProtectionProfileInstr {
	if len(iter.instrs) == 0 {
		panic("no more instructions")
	}

	for {
		if len(iter.instrs[0]) == 0 {
			iter.instrs = iter.instrs[1:]
			return &pcrProtectionProfileEndProfileInstr{}
		}

		instr := iter.instrs[0][0]
		iter.instrs[0] = iter.instrs[0][1:]

		switch i := instr.(type) {
		case *pcrProtectionProfileAddProfileORInstr:
			if len(i.profiles) == 0 {
				// If this is an empty branch point, don't return this instruction because there
				// won't be a corresponding *EndProfileInstr
				continue
			}
			iter.descendInToProfiles(i.profiles...)
			return instr
		default:
			return instr
		}
	}
}

// traverseInstructions returns an iterator that performs a depth first traversal through the instructions in this profile.
func (p *PCRProtectionProfile) traverseInstructions() *pcrProtectionProfileIterator {
	i := &pcrProtectionProfileIterator{}
	i.descendInToProfiles(p)
	return i
}

type pcrProtectionProfileStringifyBranchContext struct {
	index int
	total int
}

func (p *PCRProtectionProfile) String() string {
	var b bytes.Buffer

	contexts := []*pcrProtectionProfileStringifyBranchContext{{index: 0, total: 1}}
	branchStart := false

	iter := p.traverseInstructions()
	for len(contexts) > 0 {
		fmt.Fprintf(&b, "\n")
		depth := len(contexts) - 1
		if branchStart {
			branchStart = false
			fmt.Fprintf(&b, "%*sBranch %d {\n", depth*3, "", contexts[0].index)
		}

		switch i := iter.next().(type) {
		case *pcrProtectionProfileAddPCRValueInstr:
			fmt.Fprintf(&b, "%*s AddPCRValue(%v, %d, %x)", depth*3, "", i.alg, i.pcr, i.value)
		case *pcrProtectionProfileAddPCRValueFromTPMInstr:
			fmt.Fprintf(&b, "%*s AddPCRValueFromTPM(%v, %d)", depth*3, "", i.alg, i.pcr)
		case *pcrProtectionProfileExtendPCRInstr:
			fmt.Fprintf(&b, "%*s ExtendPCR(%v, %d, %x)", depth*3, "", i.alg, i.pcr, i.value)
		case *pcrProtectionProfileAddProfileORInstr:
			contexts = append([]*pcrProtectionProfileStringifyBranchContext{{index: 0, total: len(i.profiles)}}, contexts...)
			fmt.Fprintf(&b, "%*s AddProfileOR(", depth*3, "")
			branchStart = true
		case *pcrProtectionProfileEndProfileInstr:
			contexts[0].index++
			if len(contexts) > 1 {
				// This is the end of a sub-branch rather than the root profile.
				fmt.Fprintf(&b, "%*s}", depth*3, "")
			}
			switch {
			case contexts[0].index < contexts[0].total:
				// There are sibling branches to print.
				branchStart = true
			case len(contexts) > 1:
				// This is the end of a sub-branch rather than the root profile and there are no more sibling branches. Printing
				// will continue with the parent branch.
				fmt.Fprintf(&b, "\n%*s )", (depth-1)*3, "")
				fallthrough
			default:
				// Return to the parent branch's context.
				contexts = contexts[1:]
			}
		}
	}

	return b.String()
}

// pcrProtectionProfileComputeContext records state used when computing PCR values for a PCRProtectionProfile
type pcrProtectionProfileComputeContext struct {
	parent *pcrProtectionProfileComputeContext
	values pcrValuesList
}

// handleBranches is called when encountering a branch in a profile, and returns a slice of new *pcrProtectionProfileComputeContext
// instances (one for each sub-branch). At the end of each sub-branch, finishBranch must be called on the associated
// *pcrProtectionProfileComputeContext.
func (c *pcrProtectionProfileComputeContext) handleBranches(n int) (out []*pcrProtectionProfileComputeContext) {
	out = make([]*pcrProtectionProfileComputeContext, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &pcrProtectionProfileComputeContext{parent: c, values: c.values.copy()})
	}
	c.values = nil
	return
}

// finishBranch is called when encountering the end of a branch. This propagates the computed PCR values to the
// *pcrProtectionProfileComputeContext associated with the parent branch. Calling this will panic on a
// *pcrProtectionProfileComputeContext associated with the root branch.
func (c *pcrProtectionProfileComputeContext) finishBranch() {
	c.parent.values = append(c.parent.values, c.values...)
}

// isRoot returns true if this *pcrProtectionProfileComputeContext is associated with a root branch.
func (c *pcrProtectionProfileComputeContext) isRoot() bool {
	return c.parent == nil
}

// pcrProtectionProfileComputeContextStack is a stack of *pcrProtectionProfileComputeContext, with the top of the stack associated
// with the profile branch from which instructions are currently being processed.
type pcrProtectionProfileComputeContextStack []*pcrProtectionProfileComputeContext

// handleBranches is called when encountering a branch in a profile, and returns a new pcrProtectionProfileComputeContextStack with
// the top of the stack associated with the first sub-branch, from which subsequent instructions will be processed from. At the
// end of each sub-branch, finishBranch must be called.
func (s pcrProtectionProfileComputeContextStack) handleBranches(n int) pcrProtectionProfileComputeContextStack {
	newContexts := s.top().handleBranches(n)
	return pcrProtectionProfileComputeContextStack(append(newContexts, s...))
}

// finishBranch is called when encountering the end of a branch. This propagates the computed PCR values from the
// *pcrProtectionProfileComputeContext at the top of the stack to the *pcrProtectionProfileComputeContext associated with the parent
// branch, and then pops the context from the top of the stack. The new top of the stack corresponds to either a sibling branch or
// the parent branch, from which subsequent instructions will be processed from.
func (s pcrProtectionProfileComputeContextStack) finishBranch() pcrProtectionProfileComputeContextStack {
	s.top().finishBranch()
	return s[1:]
}

// top returns the *pcrProtectionProfileComputeContext at the top of the stack, which is associated with the branch that instructions
// are currently being processed from.
func (s pcrProtectionProfileComputeContextStack) top() *pcrProtectionProfileComputeContext {
	return s[0]
}

// ComputePCRValues computes PCR values for this PCRProtectionProfile, returning one set of PCR values
// for each complete branch. The returned list of PCR values is not de-duplicated.
func (p *PCRProtectionProfile) ComputePCRValues(tpm *tpm2.TPMContext) ([]tpm2.PCRValues, error) {
	contexts := pcrProtectionProfileComputeContextStack{{values: pcrValuesList{make(tpm2.PCRValues)}}}

	iter := p.traverseInstructions()
	for {
		switch i := iter.next().(type) {
		case *pcrProtectionProfileAddPCRValueInstr:
			contexts.top().values.setValue(i.alg, i.pcr, i.value)
		case *pcrProtectionProfileAddPCRValueFromTPMInstr:
			if tpm == nil {
				return nil, fmt.Errorf("cannot read current value of PCR %d from bank %v: no TPM context", i.pcr, i.alg)
			}
			_, v, err := tpm.PCRRead(tpm2.PCRSelectionList{{Hash: i.alg, Select: []int{i.pcr}}})
			if err != nil {
				return nil, xerrors.Errorf("cannot read current value of PCR %d from bank %v: %w", i.pcr, i.alg, err)
			}
			contexts.top().values.setValue(i.alg, i.pcr, v[i.alg][i.pcr])
		case *pcrProtectionProfileExtendPCRInstr:
			contexts.top().values.extendValue(i.alg, i.pcr, i.value)
		case *pcrProtectionProfileAddProfileORInstr:
			// As this is a depth-first traversal, processing of this branch is parked when a AddProfileOR instruction is encountered.
			// Subsequent instructions will be from each of the sub-branches in turn.
			contexts = contexts.handleBranches(len(i.profiles))
		case *pcrProtectionProfileEndProfileInstr:
			if contexts.top().isRoot() {
				// This is the end of the profile
				return []tpm2.PCRValues(contexts.top().values), nil
			}
			contexts = contexts.finishBranch()
		}
	}
}

// ComputePCRDigests computes a PCR selection and a list of composite PCR digests from this PCRProtectionProfile (one composite digest per
// complete branch). The returned list of PCR digests is de-duplicated.
func (p *PCRProtectionProfile) ComputePCRDigests(tpm *tpm2.TPMContext, alg tpm2.HashAlgorithmId) (tpm2.PCRSelectionList, tpm2.DigestList, error) {
	// Compute the sets of PCR values for all branches
	values, err := p.ComputePCRValues(tpm)
	if err != nil {
		return nil, nil, err
	}

	// Compute the PCR selection for this profile from the first branch.
	pcrs := values[0].SelectionList()

	// Compute the PCR digests for all branches, making sure that they all contain values for the same sets of PCRs.
	var pcrDigests tpm2.DigestList
	for _, v := range values {
		p, digest, _ := tpm2.ComputePCRDigestSimple(alg, v)
		if !p.Equal(pcrs) {
			return nil, nil, errors.New("not all branches contain values for the same sets of PCRs")
		}
		pcrDigests = append(pcrDigests, digest)
	}

	var uniquePcrDigests tpm2.DigestList
	for _, d := range pcrDigests {
		found := false
		for _, f := range uniquePcrDigests {
			if bytes.Equal(d, f) {
				found = true
				break
			}
		}
		if found {
			continue
		}
		uniquePcrDigests = append(uniquePcrDigests, d)
	}

	return pcrs, uniquePcrDigests, nil
}
