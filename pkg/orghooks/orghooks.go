// Package orghooks lets packages react to organisation-wide events without
// importing each other (no import cycles): the platform's "reset
// organisation" calls every registered reset hook inside its transaction.
package orghooks

import (
	"sync"

	"gorm.io/gorm"
)

// ResetOptions describes what an organisation reset clears.
type ResetOptions struct {
	KeepCustomers bool // customers / guests are kept
	KeepCatalogue bool // products, services, stock items, recipes, locations, suppliers are kept
	KeepStaff     bool // users other than the owners are kept
}

// ResetHook clears a package's data for org inside tx. It must only touch
// rows of that organisation and return an error to roll the reset back.
type ResetHook struct {
	Name string
	Fn   func(tx *gorm.DB, org string, opts ResetOptions) error
}

var (
	mu    sync.Mutex
	hooks []ResetHook
)

// RegisterReset adds a reset hook (call from init or route registration).
func RegisterReset(name string, fn func(tx *gorm.DB, org string, opts ResetOptions) error) {
	mu.Lock()
	defer mu.Unlock()
	hooks = append(hooks, ResetHook{Name: name, Fn: fn})
}

// ResetHooks returns the registered hooks in registration order.
func ResetHooks() []ResetHook {
	mu.Lock()
	defer mu.Unlock()
	return append([]ResetHook(nil), hooks...)
}
