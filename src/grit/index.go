package grit

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	git "gopkg.in/src-d/go-git.v4"

	"github.com/boltdb/bolt"
)

// Index is an index of repository locations.
type Index struct {
	db        *bolt.DB
	providers []*Provider
}

// OpenIndex opens the index database at path f.
func OpenIndex(f string, p []*Provider) (*Index, error) {
	if err := os.MkdirAll(path.Dir(f), 0755); err != nil {
		return nil, err
	}

	db, err := bolt.Open(f, 0644, nil)
	if err != nil {
		return nil, err
	}

	return &Index{db, p}, nil
}

// Find returns a list of paths containing a repository with the given slug.
func (i *Index) Find(slug string) (dirs []string, err error) {
	err = i.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if meta == nil {
			return nil
		}

		active := meta.Get(activeBucketKey)
		if active == nil {
			return nil
		}

		b := tx.Bucket(active)
		if b == nil {
			return nil
		}

		sub := b.Bucket([]byte(slug))
		if sub == nil {
			return nil
		}

		return sub.ForEach(func(dir []byte, _ []byte) error {
			dirs = append(dirs, string(dir))
			return nil
		})
	})

	return
}

// Add a clone path to the index.
func (i *Index) Add(dir string) error {
	fn, err := i.indexer(dir)
	if fn == nil {
		return err
	}

	return i.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}

		active := meta.Get(activeBucketKey)
		if active == nil {
			seq, _ := meta.NextSequence()
			active = []byte(fmt.Sprintf("repos-%d", seq))
			err = meta.Put(activeBucketKey, active)
			if err != nil {
				return err
			}
		}

		b, err := tx.CreateBucketIfNotExists(active)
		if err != nil {
			return err
		}

		return fn(tx, b)
	})
}

// Rebuild the entire index.
func (i *Index) Rebuild(paths ...string) error {
	defer i.gc()

	for _, p := range i.providers {
		paths = append(paths, p.BasePath)
	}

	bucket, err := i.newBucket()
	if err != nil {
		return err
	}

	pending := 0
	errors := make(chan error)

	for _, p := range paths {
		_ = filepath.Walk(p, func(dir string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return nil
			}

			if _, err := os.Stat(path.Join(dir, ".git")); err != nil {
				return nil
			}

			pending++
			go func() {
				fn, err := i.indexer(dir)
				if fn != nil {
					err = i.db.Batch(func(tx *bolt.Tx) error {
						return fn(tx, tx.Bucket(bucket))
					})
				}
				errors <- err
			}()

			return filepath.SkipDir
		})
	}

	for e := range errors {
		if e != nil {
			err = e
		}
		pending--
		if pending == 0 {
			close(errors)
		}
	}

	if err == nil {
		return i.activateBucket(bucket)
	}

	return err
}

// Close closes the index.
func (i *Index) Close() {
	_ = i.db.Close()
}

// WriteTo dumps a string representation of the database to w.
func (i *Index) WriteTo(w io.Writer) (int64, error) {
	var size int
	return int64(size), i.db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			n, err := writeBucket(w, b, name, "")
			size += n
			return err
		})
	})
}

func writeBucket(w io.Writer, b *bolt.Bucket, name []byte, indent string) (int, error) {
	size, err := fmt.Fprintf(w, "%s* %s\n", indent, name)

	if err == nil {
		err = b.ForEach(func(k []byte, v []byte) error {
			var (
				n int
				e error
			)

			if v == nil {
				n, e = writeBucket(w, b.Bucket(k), k, indent+"  ")
			} else if len(v) == 0 {
				n, e = fmt.Fprintf(w, "%s  - '%s' (empty)\n", indent, k)
			} else {
				n, e = fmt.Fprintf(w, "%s  - '%s' : '%s'\n", indent, k, v)
			}

			size += n
			return e
		})
	}

	return size, err
}

func (i *Index) newBucket() (name []byte, err error) {
	err = i.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}

		seq, _ := meta.NextSequence()
		name = []byte(fmt.Sprintf("repos-%d", seq))

		_, err = tx.CreateBucket(name)
		return err
	})

	return
}

func (i *Index) activateBucket(name []byte) error {
	return i.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}

		return meta.Put(activeBucketKey, name)
	})
}

func (i *Index) gc() {
	_ = i.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if meta == nil {
			return nil
		}

		active := meta.Get(activeBucketKey)

		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			if bytes.Equal(name, active) || bytes.Equal(name, metaBucket) {
				return nil
			}

			return tx.DeleteBucket(name)
		})
	})
}

func (i *Index) indexer(dir string) (func(*bolt.Tx, *bolt.Bucket) error, error) {
	repo, err := git.PlainOpen(dir)
	if err != nil {
		return nil, err
	}

	slugs, err := i.slugs(repo)
	if err != nil {
		return nil, err
	} else if len(slugs) == 0 {
		return nil, nil
	}

	return func(tx *bolt.Tx, b *bolt.Bucket) error {
		for _, slug := range slugs {
			sub, err := b.CreateBucketIfNotExists([]byte(slug))
			if err != nil {
				return err
			}

			if err := sub.Put([]byte(dir), []byte{}); err != nil {
				return err
			}
		}

		return nil
	}, nil
}

func (i *Index) slugs(r *git.Repository) (slugs []string, err error) {
	var s []string
	for _, p := range i.providers {
		s, err = p.Driver.Slugs(r)
		if err != nil {
			return
		}

		slugs = append(slugs, s...)
	}

	return
}

var (
	metaBucket      = []byte("meta")
	activeBucketKey = []byte("active")
)