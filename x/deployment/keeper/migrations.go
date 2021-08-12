package keeper

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	v013 "github.com/ovrclk/akash/x/deployment/legacy/v013"
)

// Migrator is a struct for handling in-place store migrations.
type Migrator struct {
	keeper Keeper
}

// NewMigrator returns a new Migrator.
func NewMigrator(k IKeeper) Migrator {
	return Migrator{keeper: k.(Keeper)}
}

// Migrate1to2 migrates from version 1 to 2.
func (m Migrator) Migrate1to2(ctx sdk.Context) error {
	return v013.MigrateStore(ctx, m.keeper.skey)
}