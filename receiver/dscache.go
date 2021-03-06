//
// Copyright 2016 Gregory Trubetskoy. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package receiver

import (
	"fmt"
	"sync"
	"time"

	"github.com/tgres/tgres/cluster"
	"github.com/tgres/tgres/serde"
)

// A collection of data sources kept by name (string).
type dsCache struct {
	sync.RWMutex
	byIdent map[string]*cachedDs
	db      serde.Fetcher
	dsf     dsFlusherBlocking
	finder  MatchingDSSpecFinder
	clstr   clusterer
}

// Returns a new dsCache object.
func newDsCache(db serde.Fetcher, finder MatchingDSSpecFinder, dsf dsFlusherBlocking) *dsCache {
	d := &dsCache{
		byIdent: make(map[string]*cachedDs),
		db:      db,
		finder:  finder,
		dsf:     dsf,
	}
	return d
}

// getByName rlocks and gets a DS pointer.
func (d *dsCache) getByIdent(ident serde.Ident) *cachedDs {
	d.RLock()
	defer d.RUnlock()
	return d.byIdent[ident.String()]
}

// Insert locks and inserts a DS.
func (d *dsCache) insert(cds *cachedDs) {
	d.Lock()
	defer d.Unlock()
	d.byIdent[cds.Ident().String()] = cds
}

// Delete a DS
func (d *dsCache) delete(ident serde.Ident) {
	d.Lock()
	defer d.Unlock()
	delete(d.byIdent, ident.String())
}

func (d *dsCache) preLoad() error {
	dss, err := d.db.FetchDataSources()
	if err != nil {
		return err
	}

	for _, ds := range dss {
		dbds, ok := ds.(serde.DbDataSourcer)
		if !ok {
			return fmt.Errorf("preLoad: ds must be a serde.DbDataSourcer")
		}
		d.insert(&cachedDs{DbDataSourcer: dbds})
		d.register(dbds)
	}

	return nil
}

// get a cached ds
func (d *dsCache) fetchOrCreateByName(ident serde.Ident) (*cachedDs, error) {
	result := d.getByIdent(ident)
	if result == nil {
		if dsSpec := d.finder.FindMatchingDSSpec(ident); dsSpec != nil {
			ds, err := d.db.FetchOrCreateDataSource(ident, dsSpec)
			if err != nil {
				return nil, err
			}
			if ds != nil {
				dbds, ok := ds.(serde.DbDataSourcer)
				if !ok {
					return nil, fmt.Errorf("fetchDataSourceByName: ds must be a serde.DbDataSourcer")
				}
				result = &cachedDs{DbDataSourcer: dbds}
				d.insert(result)
				d.register(dbds)
			}
		}
	}
	return result, nil
}

// register the rds as a DistDatum with the cluster
func (d *dsCache) register(ds serde.DbDataSourcer) {
	if d.clstr != nil {
		dds := &distDs{DbDataSourcer: ds, dsc: d}
		d.clstr.LoadDistData(func() ([]cluster.DistDatum, error) {
			return []cluster.DistDatum{dds}, nil
		})
	}
}

// cachedDs is a DS that keeps track of the last time it was flushed
// and provides a shouldByFlushed() method.
type cachedDs struct {
	serde.DbDataSourcer
	lastFlushRT time.Time // Last time this DS was flushed (actual real time).
}

func (cds *cachedDs) shouldBeFlushed(maxCachedPoints int, minCache, maxCache time.Duration) bool {
	if cds.LastUpdate().IsZero() {
		return false
	}
	pc := cds.PointCount()
	if pc > maxCachedPoints {
		return cds.lastFlushRT.Add(minCache).Before(time.Now())
	} else if pc > 0 {
		return cds.lastFlushRT.Add(maxCache).Before(time.Now())
	}
	return false
}

// distDs keeps a pointer to the dsCache so that it can delete itself
// from it, as well as access the Flusher to persist during Relinquish
type distDs struct {
	serde.DbDataSourcer
	dsc *dsCache
}

// cluster.DistDatum interface

func (ds *distDs) Relinquish() error {
	if !ds.LastUpdate().IsZero() {
		ds.dsc.dsf.flushDs(ds.DbDataSourcer, true)
		ds.dsc.delete(ds.Ident())
	}
	return nil
}

func (ds *distDs) Acquire() error {
	ds.dsc.delete(ds.Ident())
	return nil
}

func (ds *distDs) Id() int64       { return ds.DbDataSourcer.Id() }
func (ds *distDs) Type() string    { return "DataSource" }
func (ds *distDs) GetName() string { return ds.DbDataSourcer.Ident().String() }

// end cluster.DistDatum interface
