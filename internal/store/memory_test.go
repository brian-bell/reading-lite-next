package store_test

import (
	"testing"

	"github.com/bbell/reading-lite/internal/store"
	"github.com/bbell/reading-lite/internal/store/storetest"
)

func TestMemoryStoreContract(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) store.Store {
		t.Helper()
		return store.NewMemory()
	})
}

func TestMemoryBatchStoreContract(t *testing.T) {
	storetest.RunBatchContract(t, func(t *testing.T) store.BatchStore {
		t.Helper()
		return store.NewMemory()
	})
}
