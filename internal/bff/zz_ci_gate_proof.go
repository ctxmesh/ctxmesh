package bff

// This file exists ONLY to prove the CI gate can FAIL and therefore BLOCK a merge (M147.10).
// A pipeline that has never failed is not evidence of a working gate — m52.G3 records exactly that
// illusion persisting for sixty milestones. It is deleted as soon as the refusal is recorded.
func deliberatelyBroken() int { return "this is not an int" }
