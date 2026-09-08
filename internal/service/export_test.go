package service

import "github.com/OmniTrustILM/cbom-repository/internal/store"

// WithStore is a test hook: the same Service — schemas compiled once — over another
// store. Compiling the CycloneDX schemas is what New spends its time on, and the search
// tests need a fresh in-memory store per test, not fresh schemas.
func (s Service) WithStore(st store.Store) Service {
	s.store = st
	return s
}
