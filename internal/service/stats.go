package service

import (
	"context"
	"log/slog"

	cdx "github.com/CycloneDX/cyclonedx-go"
)

// CryptoStatsVersion identifies how CalculateCryptoStats counted the assets of an
// uploaded document. Uploads write it to S3 user metadata under
// store.MetaCryptoStatsVersionKey, next to the statistics themselves, together with
// store.MetaCryptoStatsTruncatedKey when the walk hit maxComponentDepth. Neither key
// appears in HTTP responses (the response shape is frozen for backward compatibility):
// paged search surfaces both as entry warnings instead — WarningCryptoStatsShallow for
// a missing version key, WarningCryptoStatsTruncated for the truncation marker.
//
//   - absent (objects uploaded before this constant existed): version 1 — a shallow
//     count over the top-level `components` array only; nested components were missed.
//   - "2": the whole `components` tree is walked (see walkComponents).
//
// Consumers that need exact counts for version-1 objects recount from the document;
// see the #138 decision (https://github.com/OmniTrustILM/cbom-repository/issues/138#issuecomment-5540800814):
// the repository does not rewrite stored objects, Core recounts during asset ingest.
const CryptoStatsVersion = "2"

// maxComponentDepth bounds how deep walkComponents descends into nested `components`.
// It mirrors Core's walker bound (DocumentScope.MAX_DEPTH = 1000) so both sides count
// the same components. Real documents nest two or three levels; the bound only guards
// against hostile input.
const maxComponentDepth = 1000

type CryptoStats struct {
	CryptoAsset CryptoAssetStats `json:"cryptoAssets"`
}

type CryptoAssetStats struct {
	Total    int        `json:"total"`
	Algo     TotalStats `json:"algorithms"`
	Cert     TotalStats `json:"certificates"`
	Protocol TotalStats `json:"protocols"`
	Related  TotalStats `json:"relatedCryptoMaterials"`
}

type TotalStats struct {
	Total int `json:"total"`
}

// CalculateCryptoStats analyzes a CycloneDX BOM and returns statistics about the
// cryptographic assets it contains. Every component of the `components` tree is
// visited — nested `components` included, to any depth up to maxComponentDepth — and
// cryptographic assets are counted by type (algorithm, certificate, protocol, related
// crypto material).
//
// Scope deliberately matches Core's asset extraction: only the `components` tree is
// walked. `metadata.component`, `metadata.tools`, `formulation` and `services` are not
// part of the inventory and are not counted.
//
// Components that are not of type ComponentTypeCryptographicAsset are skipped.
// Components missing CryptoProperties are logged as warnings and skipped.
//
// Parameters:
//   - ctx: Context for cancellation and logging
//   - bom: The CycloneDX BOM to analyze
//
// Returns a CryptoStats struct containing aggregated counts of cryptographic
// assets. If the BOM has no components or a nil Components field, a zero value
// CryptoStats struct is returned.
//
// Kept as the public entry point for callers that only need the counts; the upload
// path uses countCryptoStats directly because it also records whether the depth
// bound truncated the walk.
func CalculateCryptoStats(ctx context.Context, bom *cdx.BOM) CryptoStats {
	cryptoStats, _ := countCryptoStats(ctx, bom)
	return cryptoStats
}

// countCryptoStats is CalculateCryptoStats plus the one fact the public signature cannot
// carry: truncated is true when the walk stopped descending at maxComponentDepth, so the
// counts are a lower bound. Uploads persist that as store.MetaCryptoStatsTruncatedKey.
func countCryptoStats(ctx context.Context, bom *cdx.BOM) (CryptoStats, bool) {
	var cryptoStats CryptoStats
	if bom.Components == nil {
		slog.WarnContext(ctx, "BOM has nil root level 'Components' field.", slog.String("serialNumber", bom.SerialNumber))
		return cryptoStats, false
	}

	components, truncated := walkComponents(ctx, *bom.Components)
	for _, component := range components {
		if component.Type != cdx.ComponentTypeCryptographicAsset {
			continue
		}

		if component.CryptoProperties == nil {
			slog.WarnContext(ctx, "Component is a crypto asset but has a nil CryptoProperties field. Skipping.", slog.String("bom-ref", component.BOMRef))
			continue
		}
		cryptoStats.CryptoAsset.Total += 1

		switch component.CryptoProperties.AssetType {
		case cdx.CryptoAssetTypeAlgorithm:
			cryptoStats.CryptoAsset.Algo.Total += 1

		case cdx.CryptoAssetTypeCertificate:
			cryptoStats.CryptoAsset.Cert.Total += 1

		case cdx.CryptoAssetTypeProtocol:
			cryptoStats.CryptoAsset.Protocol.Total += 1

		case cdx.CryptoAssetTypeRelatedCryptoMaterial:
			cryptoStats.CryptoAsset.Related.Total += 1
		}
	}
	return cryptoStats, truncated
}

// componentFrame is one pending node of the iterative walk: the component and the
// nesting depth it sits at (top-level components are depth 1).
type componentFrame struct {
	component *cdx.Component
	depth     int
}

// walkComponents returns every component of the tree rooted at the given top-level
// components — nested ones included — depth-first in document order (a component,
// then its subtree, then its next sibling).
//
// The walk is iterative rather than recursive and bounded by maxComponentDepth:
// `components` nests arbitrarily in every CycloneDX version, so a hostile document
// nested thousands deep must not exhaust the stack. Components at the bound are still
// returned; their children are not visited, and that is logged once per document.
//
// truncated reports whether that happened — a component at the bound actually had
// children — which makes the returned set, and any count built from it, a lower bound.
func walkComponents(ctx context.Context, top []cdx.Component) ([]*cdx.Component, bool) {
	found := make([]*cdx.Component, 0, len(top))
	pending := make([]componentFrame, 0, len(top))
	pushChildren := func(components *[]cdx.Component, depth int) {
		if components == nil {
			return
		}
		// Pushed in reverse so that the stack pops siblings in document order.
		for i := len(*components) - 1; i >= 0; i-- {
			pending = append(pending, componentFrame{component: &(*components)[i], depth: depth})
		}
	}

	pushChildren(&top, 1)
	truncated := false
	for len(pending) > 0 {
		frame := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		found = append(found, frame.component)

		if frame.depth >= maxComponentDepth {
			if frame.component.Components != nil && len(*frame.component.Components) > 0 {
				if !truncated {
					slog.WarnContext(ctx, "Component tree nests deeper than the supported maximum. Deeper components are not counted.",
						slog.String("bom-ref", frame.component.BOMRef), slog.Int("max-depth", maxComponentDepth))
				}
				truncated = true
			}
			continue
		}
		pushChildren(frame.component.Components, frame.depth+1)
	}
	return found, truncated
}
