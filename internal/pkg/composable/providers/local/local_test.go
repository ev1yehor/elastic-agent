// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package local

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elastic/elastic-agent/internal/pkg/composable"
	ctesting "github.com/elastic/elastic-agent/internal/pkg/composable/testing"
	"github.com/elastic/elastic-agent/internal/pkg/config"
)

func TestContextProvider(t *testing.T) {
	mapping := map[string]interface{}{
		"key1": "value1",
		"key2": "value2",
	}
	cfg, err := config.NewConfigFrom(map[string]interface{}{
		"vars": mapping,
	})
	require.NoError(t, err)
	builder, _ := composable.Providers.GetContextProvider("local")
	provider, err := builder(nil, cfg, true)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	comm := ctesting.NewContextComm(ctx)
	err = provider.Run(ctx, comm)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	assert.Equal(t, mapping, comm.Current())
}
