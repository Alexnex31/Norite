package auth

// provedFactor builds a satisfied proof for a test that is not about the second factor.
//
// Constructible here only because these tests live in the package. That is the boundary of the
// enforcement and it is worth being explicit about: factorProof stops a *caller in another package*, and
// stops anyone in this one from doing it by accident, but it cannot stop somebody in this package writing
// the literal deliberately. Everything that mints a session outside a test obtains its proof from
// factorSatisfied or proveFactor; this exists so tests about sessions do not have to enroll a factor first.
func provedFactor(userID int64) factorProof {
	return factorProof{userID: userID, proved: true}
}
