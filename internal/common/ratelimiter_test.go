// Copyright (C) 2019 Nicola Murino
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRateLimiterConfig(t *testing.T) {
	config := RateLimiterConfig{}
	err := config.validate()
	require.Error(t, err)
	config.Burst = 1
	config.Period = 10
	err = config.validate()
	require.Error(t, err)
	config.Period = 1000
	config.Type = 100
	err = config.validate()
	require.Error(t, err)
	config.Type = int(rateLimiterTypeSource)
	config.EntriesSoftLimit = 0
	err = config.validate()
	require.Error(t, err)
	config.EntriesSoftLimit = 150
	config.EntriesHardLimit = 0
	err = config.validate()
	require.Error(t, err)
	config.EntriesHardLimit = 200
	config.Protocols = []string{"unsupported protocol"}
	err = config.validate()
	require.Error(t, err)
	config.Protocols = rateLimiterProtocolValues
	err = config.validate()
	require.NoError(t, err)

	limiter := config.getLimiter()
	require.Equal(t, 500*time.Millisecond, limiter.maxDelay)
	require.Nil(t, limiter.globalBucket)
	config.Type = int(rateLimiterTypeGlobal)
	config.Average = 1
	config.Period = 10000
	limiter = config.getLimiter()
	require.Equal(t, 5*time.Second, limiter.maxDelay)
	require.NotNil(t, limiter.globalBucket)
	config.Period = 100000
	limiter = config.getLimiter()
	require.Equal(t, 10*time.Second, limiter.maxDelay)
	config.Period = 500
	config.Average = 1
	limiter = config.getLimiter()
	require.Equal(t, 250*time.Millisecond, limiter.maxDelay)
}

func TestRateLimiter(t *testing.T) {
	config := RateLimiterConfig{
		Average:   1,
		Period:    1000,
		Burst:     1,
		Type:      int(rateLimiterTypeGlobal),
		Protocols: rateLimiterProtocolValues,
	}
	limiter := config.getLimiter()
	_, err := limiter.Wait("", ProtocolFTP)
	require.NoError(t, err)
	_, err = limiter.Wait("", ProtocolSSH)
	require.Error(t, err)

	config.Type = int(rateLimiterTypeSource)
	config.GenerateDefenderEvents = true
	config.EntriesSoftLimit = 5
	config.EntriesHardLimit = 10
	limiter = config.getLimiter()

	source := "192.168.1.2"
	_, err = limiter.Wait(source, ProtocolSSH)
	require.NoError(t, err)
	_, err = limiter.Wait(source, ProtocolSSH)
	require.Error(t, err)
	// a different source should work
	_, err = limiter.Wait(source+"1", ProtocolSSH)
	require.NoError(t, err)

	config.Burst = 0
	limiter = config.getLimiter()
	_, err = limiter.Wait(source, ProtocolSSH)
	require.ErrorIs(t, err, errReserve)
}

func TestLimiterCleanup(t *testing.T) {
	config := RateLimiterConfig{
		Average:          100,
		Period:           1000,
		Burst:            1,
		Type:             int(rateLimiterTypeSource),
		Protocols:        rateLimiterProtocolValues,
		EntriesSoftLimit: 1,
		EntriesHardLimit: 3,
	}
	limiter := config.getLimiter()
	source1 := "10.8.0.1"
	source2 := "10.8.0.2"
	source3 := "10.8.0.3"
	source4 := "10.8.0.4"
	_, err := limiter.Wait(source1, ProtocolSSH)
	assert.NoError(t, err)
	time.Sleep(20 * time.Millisecond)
	_, err = limiter.Wait(source2, ProtocolSSH)
	assert.NoError(t, err)
	time.Sleep(20 * time.Millisecond)
	assert.Len(t, limiter.buckets.buckets, 2)
	_, ok := limiter.buckets.buckets[source1]
	assert.True(t, ok)
	_, ok = limiter.buckets.buckets[source2]
	assert.True(t, ok)
	_, err = limiter.Wait(source3, ProtocolSSH)
	assert.NoError(t, err)
	assert.Len(t, limiter.buckets.buckets, 3)
	_, ok = limiter.buckets.buckets[source1]
	assert.True(t, ok)
	_, ok = limiter.buckets.buckets[source2]
	assert.True(t, ok)
	_, ok = limiter.buckets.buckets[source3]
	assert.True(t, ok)
	time.Sleep(20 * time.Millisecond)
	_, err = limiter.Wait(source4, ProtocolSSH)
	assert.NoError(t, err)
	assert.Len(t, limiter.buckets.buckets, 2)
	_, ok = limiter.buckets.buckets[source3]
	assert.True(t, ok)
	_, ok = limiter.buckets.buckets[source4]
	assert.True(t, ok)
}
