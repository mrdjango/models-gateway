package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateTensorGridCreditSyncConfig(t *testing.T) {
	valid := map[string]string{
		"TENSORGRID_INTEGRATION_SECRET": "0123456789abcdef0123456789abcdef",
		"TENSORGRID_CALLBACK_BASE_URL":  "http://tensorgrid-backend:8000",
		"NODE_TYPE":                     "master",
		"BATCH_UPDATE_ENABLED":          "false",
	}
	for name, value := range valid {
		t.Setenv(name, value)
	}
	require.NoError(t, ValidateTensorGridCreditSyncConfig())

	tests := []struct {
		name          string
		environment   string
		value         string
		expectedError string
	}{
		{
			name:          "short integration secret",
			environment:   "TENSORGRID_INTEGRATION_SECRET",
			value:         "too-short",
			expectedError: "TENSORGRID_INTEGRATION_SECRET",
		},
		{
			name:          "relative callback URL",
			environment:   "TENSORGRID_CALLBACK_BASE_URL",
			value:         "tensorgrid-backend:8000",
			expectedError: "TENSORGRID_CALLBACK_BASE_URL",
		},
		{
			name:          "slave node",
			environment:   "NODE_TYPE",
			value:         "slave",
			expectedError: "NODE_TYPE=master",
		},
		{
			name:          "batch updates enabled",
			environment:   "BATCH_UPDATE_ENABLED",
			value:         "true",
			expectedError: "BATCH_UPDATE_ENABLED=false",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for name, value := range valid {
				t.Setenv(name, value)
			}
			t.Setenv(test.environment, test.value)
			err := ValidateTensorGridCreditSyncConfig()
			require.Error(t, err)
			assert.ErrorContains(t, err, test.expectedError)
		})
	}
}
