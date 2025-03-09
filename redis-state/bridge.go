// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package redisstate

import (
	"github.com/erigontech/erigon/core/state"
)

// init initializes the Redis integration with the core/state package
func init() {
	// Register the Redis writer factory with the state package
	// This allows the state package to create Redis writers when needed
	// without introducing circular dependencies
	factoryFn := GetRedisWriterFactoryFn()
	state.SetRedisWriterFactory(factoryFn)
}