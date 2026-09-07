package service

import (
	"context"
	"fmt"
	"testing"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/stretchr/testify/require"
)

// asset returns a crypto-asset component with the given name and children — enough of a
// component for the walk, which looks only at names and nested `components`.
func asset(name string, children ...cdx.Component) cdx.Component {
	c := cdx.Component{
		Type:             cdx.ComponentTypeCryptographicAsset,
		Name:             name,
		CryptoProperties: &cdx.CryptoProperties{AssetType: cdx.CryptoAssetTypeAlgorithm},
	}
	if len(children) > 0 {
		c.Components = &children
	}
	return c
}

func walkNames(components []*cdx.Component) []string {
	names := make([]string, 0, len(components))
	for _, c := range components {
		names = append(names, c.Name)
	}
	return names
}

// The walk is depth-first in document order — a component, then its whole subtree, then
// its next sibling — which is the order Core's walker uses, so per-type counts line up
// with Core's extraction. A tree that fits inside the depth bound is never truncated.
func TestWalkComponents_DocumentOrder(t *testing.T) {
	top := []cdx.Component{
		asset("a", asset("a.1", asset("a.1.1")), asset("a.2")),
		asset("b", asset("b.1")),
	}

	found, truncated := walkComponents(context.Background(), top)
	require.Equal(t, []string{"a", "a.1", "a.1.1", "a.2", "b", "b.1"}, walkNames(found))
	require.False(t, truncated)
}

// A chain nested past the bound reports truncated: the walk returns the components down
// to the bound and stops descending, so the counts built from it are a lower bound.
func TestWalkComponents_DepthBoundTruncates(t *testing.T) {
	const chain = 1005
	current := asset(fmt.Sprintf("level-%d", chain))
	for level := chain - 1; level >= 1; level-- {
		current = asset(fmt.Sprintf("level-%d", level), current)
	}

	found, truncated := walkComponents(context.Background(), []cdx.Component{current})
	require.True(t, truncated)
	require.Len(t, found, maxComponentDepth)
	require.Equal(t, "level-1", found[0].Name)
	require.Equal(t, fmt.Sprintf("level-%d", maxComponentDepth), found[len(found)-1].Name)
}

// A component sitting exactly at the bound with no children of its own is not
// truncation: nothing was left uncounted.
func TestWalkComponents_BoundWithoutChildrenIsNotTruncated(t *testing.T) {
	current := asset(fmt.Sprintf("level-%d", maxComponentDepth))
	for level := maxComponentDepth - 1; level >= 1; level-- {
		current = asset(fmt.Sprintf("level-%d", level), current)
	}

	found, truncated := walkComponents(context.Background(), []cdx.Component{current})
	require.False(t, truncated)
	require.Len(t, found, maxComponentDepth)
}
