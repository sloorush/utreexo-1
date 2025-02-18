package utreexo

import (
	"encoding/hex"
	"fmt"
	"sort"

	"golang.org/x/exp/slices"
)

// Proof is the inclusion-proof for multiple leaves.
type Proof struct {
	// Targets are the ist of leaf locations to delete and they are the bottommost leaves.
	// With the tree below, the Targets can only consist of one of these: 02, 03, 04.
	//
	// 06
	// |-------\
	// 04      05
	// |---\   |---\
	//         02  03
	Targets []uint64

	// All the nodes in the tree that are needed to hash up to the root of
	// the tree. Here, the root is 06. If Targets are [00, 01], then Proof
	// would be [05] as you need 04 and 05 to hash to 06. 04 can be calculated
	// by hashing 00 and 01.
	//
	// 06
	// |-------\
	// 04      05
	// |---\   |---\
	// 00  01  02  03
	Proof []Hash
}

// ToString for debugging, shows the proof
func (p *Proof) ToString() string {
	targs := make([]uint64, len(p.Targets))
	copy(targs, p.Targets)
	sort.Slice(targs, func(a, b int) bool { return targs[a] < targs[b] })

	s := fmt.Sprintf("%d targets: ", len(targs))
	for _, t := range targs {
		s += fmt.Sprintf("%d ", t)
	}
	s += fmt.Sprintf("\n%d proofs: ", len(p.Proof))
	for _, p := range p.Proof {
		s += fmt.Sprintf("%04x\t", p[:8])
	}
	s += "\n"
	return s
}

func (p *Pollard) Prove(hashes []Hash) (Proof, error) {
	// No hashes to prove means that the proof is empty. An empty
	// pollard also has an empty proof.
	if len(hashes) == 0 || p.numLeaves == 0 {
		return Proof{}, nil
	}
	// A Pollard with 1 leaf has no proof and only 1 target.
	if p.numLeaves == 1 {
		return Proof{Targets: []uint64{0}}, nil
	}

	var proof Proof
	proof.Targets = make([]uint64, len(hashes))

	// Grab the positions of the hashes that are to be proven.
	for i, wanted := range hashes {
		node, ok := p.nodeMap[wanted.mini()]
		if !ok {
			return proof, fmt.Errorf("Prove error: hash %s not found",
				hex.EncodeToString(wanted[:]))
		}
		proof.Targets[i] = p.calculatePosition(node)
	}

	// Sort the targets as the proof hashes need to be sorted.
	//
	// TODO find out if sorting and losing in-block position information hurts
	// locality or performance.
	sortedTargets := make([]uint64, len(proof.Targets))
	copy(sortedTargets, proof.Targets)
	sort.Slice(sortedTargets, func(a, b int) bool { return sortedTargets[a] < sortedTargets[b] })

	// Get the positions of all the hashes that are needed to prove the targets
	proofPositions := proofPositions(sortedTargets, p.numLeaves, treeRows(p.numLeaves))

	// Fetch all the proofs from the accumulator.
	proof.Proof = make([]Hash, len(proofPositions))
	for i, proofPos := range proofPositions {
		hash := p.getHash(proofPos)
		if hash == empty {
			return Proof{}, fmt.Errorf("Prove error: couldn't read position %d", proofPos)
		}
		proof.Proof[i] = hash
	}

	return proof, nil
}

type hashAndPos struct {
	hash Hash
	pos  uint64
}

// toHashAndPos returns a slice of hash and pos that's sorted.
func toHashAndPos(targets []uint64, hashes []Hash) []hashAndPos {
	hnp := make([]hashAndPos, len(hashes))

	for i := range hnp {
		hnp[i].hash = hashes[i]
		hnp[i].pos = targets[i]
	}

	// No guarantee that the targets and the delHashes are in order. Sort them
	// before processing.
	sort.Slice(hnp, func(a, b int) bool { return hnp[a].pos < hnp[b].pos })

	return hnp
}

// Verify calculates the root hashes from the passed in proof and delHashes and
// compares it against the current roots in the pollard.
func (p *Pollard) Verify(delHashes []Hash, proof Proof) error {
	if len(delHashes) == 0 {
		return nil
	}

	if len(delHashes) != len(proof.Targets) {
		return fmt.Errorf("Pollard.Verify fail. Was given %d targets but got %d hashes",
			len(proof.Targets), len(delHashes))
	}

	rootCandidates := calculateRoots(p.numLeaves, delHashes, proof)
	if len(rootCandidates) == 0 {
		return fmt.Errorf("Pollard.Verify fail. No roots calculated "+
			"but have %d deletions", len(delHashes))
	}

	rootMatches := 0
	for i := range p.roots {
		if len(rootCandidates) > rootMatches &&
			p.roots[len(p.roots)-(i+1)].data == rootCandidates[rootMatches] {
			rootMatches++
		}
	}
	// Error out if all the rootCandidates do not have a corresponding
	// polnode with the same hash.
	if len(rootCandidates) != rootMatches {
		// The proof is invalid because some root candidates were not
		// included in `roots`.
		err := fmt.Errorf("Pollard.Verify fail. Have %d roots but only "+
			"matched %d roots", len(rootCandidates), rootMatches)
		return err
	}

	return nil
}

// calculateRoots calculates and returns the root hashes.
func calculateRoots(numLeaves uint64, delHashes []Hash, proof Proof) []Hash {
	totalRows := treeRows(numLeaves)

	// Where all the root hashes that we've calculated will go to.
	calculatedRootHashes := make([]Hash, 0, numRoots(numLeaves))

	// Where all the parent hashes we've calculated in a given row will go to.
	nextProves := make([]hashAndPos, 0, len(delHashes))

	// These are the leaves to be proven. Each represent a position and the
	// hash of a leaf.
	toProve := toHashAndPos(proof.Targets, delHashes)

	// Separate index for the hashes in the passed in proof.
	proofHashIdx := 0
	for row := 0; row <= int(totalRows); row++ {
		extractedProves := extractRowHash(toProve, totalRows, uint8(row))

		proves := mergeSortedHashAndPos(nextProves, extractedProves)
		nextProves = nextProves[:0]

		for i := 0; i < len(proves); i++ {
			prove := proves[i]

			// This means we hashed all the way to the top of this subtree.
			if isRootPosition(prove.pos, numLeaves, totalRows) {
				calculatedRootHashes = append(calculatedRootHashes, prove.hash)
				continue
			}

			// Check if the next prove is the sibling of this prove.
			if i+1 < len(proves) && rightSib(prove.pos) == proves[i+1].pos {
				nextProve := hashAndPos{
					hash: parentHash(prove.hash, proves[i+1].hash),
					pos:  parent(prove.pos, totalRows),
				}
				nextProves = append(nextProves, nextProve)

				i++ // Increment one more since we procesed another prove.
			} else {
				// If the next prove isn't the sibling of this prove, we fetch
				// the next proof hash to calculate the parent.
				hash := proof.Proof[proofHashIdx]
				proofHashIdx++

				nextProve := hashAndPos{pos: parent(prove.pos, totalRows)}
				if isLeftNiece(prove.pos) {
					nextProve.hash = parentHash(prove.hash, hash)
				} else {
					nextProve.hash = parentHash(hash, prove.hash)
				}

				nextProves = append(nextProves, nextProve)
			}
		}
	}

	return calculatedRootHashes
}

func mergeSortedHashAndPos(a []hashAndPos, b []hashAndPos) (c []hashAndPos) {
	maxa := len(a)
	maxb := len(b)

	// shortcuts:
	if maxa == 0 {
		return b
	}
	if maxb == 0 {
		return a
	}

	// make it (potentially) too long and truncate later
	c = make([]hashAndPos, maxa+maxb)

	idxa, idxb := 0, 0
	for j := 0; j < len(c); j++ {
		// if we're out of a or b, just use the remainder of the other one
		if idxa >= maxa {
			// a is done, copy remainder of b
			j += copy(c[j:], b[idxb:])
			c = c[:j] // truncate empty section of c
			break
		}
		if idxb >= maxb {
			// b is done, copy remainder of a
			j += copy(c[j:], a[idxa:])
			c = c[:j] // truncate empty section of c
			break
		}

		vala, valb := a[idxa], b[idxb]
		if vala.pos < valb.pos { // a is less so append that
			c[j] = vala
			idxa++
		} else if vala.pos > valb.pos { // b is less so append that
			c[j] = valb
			idxb++
		} else { // they're equal
			c[j] = vala
			idxa++
			idxb++
		}
	}
	return
}

func mergeSortedNodeAndPos(a []nodeAndPos, b []nodeAndPos) (c []nodeAndPos) {
	maxa := len(a)
	maxb := len(b)

	// shortcuts:
	if maxa == 0 {
		return b
	}
	if maxb == 0 {
		return a
	}

	// make it (potentially) too long and truncate later
	c = make([]nodeAndPos, maxa+maxb)

	idxa, idxb := 0, 0
	for j := 0; j < len(c); j++ {
		// if we're out of a or b, just use the remainder of the other one
		if idxa >= maxa {
			// a is done, copy remainder of b
			j += copy(c[j:], b[idxb:])
			c = c[:j] // truncate empty section of c
			break
		}
		if idxb >= maxb {
			// b is done, copy remainder of a
			j += copy(c[j:], a[idxa:])
			c = c[:j] // truncate empty section of c
			break
		}

		vala, valb := a[idxa], b[idxb]
		if vala.pos < valb.pos { // a is less so append that
			c[j] = vala
			idxa++
		} else if vala.pos > valb.pos { // b is less so append that
			c[j] = valb
			idxb++
		} else { // they're equal
			c[j] = vala
			idxa++
			idxb++
		}
	}
	return
}

func extractRowHash(toProve []hashAndPos, forestRows, rowToExtract uint8) []hashAndPos {
	if len(toProve) < 0 {
		return []hashAndPos{}
	}

	start := -1
	end := 0

	for i := 0; i < len(toProve); i++ {
		if detectRow(toProve[i].pos, forestRows) == rowToExtract {
			if start == -1 {
				start = i
			}

			end = i
		} else {
			// If we're not at the desired row and start has already been set
			// once, that means we've extracted everything we can. This is
			// possible because the assumption is that the toProve are sorted.
			if start != -1 {
				break
			}
		}
	}

	if start == -1 {
		return []hashAndPos{}
	}

	count := (end + 1) - start
	row := make([]hashAndPos, count)

	copy(row, toProve[start:end+1])

	return row
}

func extractRowNode(toProve []nodeAndPos, forestRows, rowToExtract uint8) []nodeAndPos {
	if len(toProve) < 0 {
		return []nodeAndPos{}
	}

	start := -1
	end := 0

	for i := 0; i < len(toProve); i++ {
		if detectRow(toProve[i].pos, forestRows) == rowToExtract {
			if start == -1 {
				start = i
			}

			end = i
		} else {
			// If we're not at the desired row and start has already been set
			// once, that means we've extracted everything we can. This is
			// possible because the assumption is that the toProve are sorted.
			if start != -1 {
				break
			}
		}
	}

	if start == -1 {
		return []nodeAndPos{}
	}

	count := (end + 1) - start
	row := make([]nodeAndPos, count)

	copy(row, toProve[start:end+1])

	return row
}

func removeDuplicateInt(uint64Slice []uint64) []uint64 {
	allKeys := make(map[uint64]bool)
	list := []uint64{}
	for _, item := range uint64Slice {
		if _, value := allKeys[item]; !value {
			allKeys[item] = true
			list = append(list, item)
		}
	}
	return list
}

// proofAfterDeletion modifies the proof so that it proves the siblings of the targets
// in this proof. Having this information allows for the calculation of roots after the
// deletion has happened.
func proofAfterDeletion(numLeaves uint64, proof Proof) ([]Hash, Proof) {
	forestRows := treeRows(numLeaves)

	// Copy the targets to avoid mutating the original. Then detwin it
	// to prep for deletion.
	targets := make([]uint64, len(proof.Targets))
	copy(targets, proof.Targets)
	sort.Slice(targets, func(a, b int) bool { return targets[a] < targets[b] })

	// Use the sorted targets to generate the positions for the proof hashes.
	proofPos := proofPositions(targets, numLeaves, forestRows)
	// Attach a position to each of the proof hashes.
	hnp := toHashAndPos(proofPos, proof.Proof)

	// This is where the new targets and its hashes will go to.
	proveTargets := make([]uint64, 0, len(targets))
	targetHashes := make([]Hash, 0, len(targets))

	// Detwin before processing.
	targets = deTwin(targets, forestRows)

	// For each of the targets, we'll try to find the sibling in the proof hashes
	// and promote it as the parent. If it's not in the proof hashes, we'll move
	// the descendatns of the existing targets and proofs of the sibling's parent
	// up by one row.
	for i := 0; i < len(targets); i++ {
		// If the target is a root, we need to add an empty hash so
		// that the stump correctly udpates the roots to include the
		// empty roots.
		if isRootPosition(targets[i], numLeaves, forestRows) {
			proveTargets = append(proveTargets, targets[i])
			targetHashes = append(targetHashes, empty)
			continue
		}

		sib := sibling(targets[i])

		// Look for the sibling in the proof hashes.
		if idx := slices.IndexFunc(hnp, func(elem hashAndPos) bool { return elem.pos == sib }); idx != -1 {
			parentPos := parent(sib, forestRows)

			proveTargets = append(proveTargets, parentPos)
			targetHashes = append(targetHashes, hnp[idx].hash)

			// Delete the sibling from hnp as this sibling is a target now, not a proof.
			hnp = append(hnp[:idx], hnp[idx+1:]...)
		} else {
			// If the sibling is not in the proof hashes or the targets,
			// the descendants of the sibling will be moving up.
			//
			// 14
			// |---------------\
			// 12              13
			// |-------\       |-------\
			// 08      09      10      11
			// |---\   |---\   |---\   |---\
			// 00  01          04  05  06  07
			//
			// In the above tree, if we're deleting 00 and 09, 09 won't be
			// able to find the sibling in the proof hashes. 01 would have moved
			// up to 08 and we'll move 08 up and to 12 as 09 is also being deleted.

			// First update the targets to their new positions.
			for j := len(proveTargets) - 1; j >= 0; j-- {
				ancestor := isAncestor(parent(sib, forestRows), proveTargets[j], forestRows)
				if ancestor {
					// We can ignore the error since we've already verified that
					// the proveTargets[j] is an ancestor of sib.
					nextPos, _ := calcNextPosition(proveTargets[j], sib, forestRows)
					proveTargets[j] = nextPos
				}
			}

			// Update the proofs as well.
			for j := len(hnp) - 1; j >= 0; j-- {
				ancestor := isAncestor(parent(sib, forestRows), hnp[j].pos, forestRows)
				if ancestor {
					// We can ignore the error since we've already verified that
					// the hnp[j] is an ancestor of sib.
					nextPos, _ := calcNextPosition(hnp[j].pos, sib, forestRows)
					hnp[j].pos = nextPos
				}
			}

			// TODO I think we can do this a different way. We need
			// the prove targets in the same order as the proof hashes
			// so if we sort and dedupe, we'd also need to sort targetHashes
			// as well.
			proveTargets = removeDuplicateInt(proveTargets)
		}
	}

	// The proof hashes should be in order before they're included in the proof.
	sort.Slice(hnp, func(a, b int) bool { return hnp[a].pos < hnp[b].pos })

	// The leftover proofs that weren't siblings of the detwined targets are
	// the new proofs for the new targets.
	hashes := make([]Hash, len(hnp))
	for i := range hnp {
		hashes[i] = hnp[i].hash
	}

	return targetHashes, Proof{proveTargets, hashes}
}
