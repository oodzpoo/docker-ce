package llbsolver

import (
	"context"
	"time"

	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/remotecache"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/exporter"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type ExporterRequest struct {
	Exporter        exporter.ExporterInstance
	CacheExporter   remotecache.Exporter
	CacheExportMode solver.CacheExportMode
}

// ResolveWorkerFunc returns default worker for the temporary default non-distributed use cases
type ResolveWorkerFunc func() (worker.Worker, error)

type Solver struct {
	solver               *solver.Solver
	resolveWorker        ResolveWorkerFunc
	frontends            map[string]frontend.Frontend
	resolveCacheImporter remotecache.ResolveCacheImporterFunc
	platforms            []specs.Platform
}

func New(wc *worker.Controller, f map[string]frontend.Frontend, cache solver.CacheManager, resolveCI remotecache.ResolveCacheImporterFunc) (*Solver, error) {
	s := &Solver{
		resolveWorker:        defaultResolver(wc),
		frontends:            f,
		resolveCacheImporter: resolveCI,
	}

	// executing is currently only allowed on default worker
	w, err := wc.GetDefault()
	if err != nil {
		return nil, err
	}
	s.platforms = w.Platforms()

	s.solver = solver.NewSolver(solver.SolverOpt{
		ResolveOpFunc: s.resolver(),
		DefaultCache:  cache,
	})
	return s, nil
}

func (s *Solver) resolver() solver.ResolveOpFunc {
	return func(v solver.Vertex, b solver.Builder) (solver.Op, error) {
		w, err := s.resolveWorker()
		if err != nil {
			return nil, err
		}
		return w.ResolveOp(v, s.Bridge(b))
	}
}

func (s *Solver) Bridge(b solver.Builder) frontend.FrontendLLBBridge {
	return &llbBridge{
		builder:              b,
		frontends:            s.frontends,
		resolveWorker:        s.resolveWorker,
		resolveCacheImporter: s.resolveCacheImporter,
		cms:                  map[string]solver.CacheManager{},
		platforms:            s.platforms,
	}
}

func (s *Solver) Solve(ctx context.Context, id string, req frontend.SolveRequest, exp ExporterRequest) (*client.SolveResponse, error) {
	j, err := s.solver.NewJob(id)
	if err != nil {
		return nil, err
	}

	defer j.Discard()

	j.SessionID = session.FromContext(ctx)

	res, err := s.Bridge(j).Solve(ctx, req)
	if err != nil {
		return nil, err
	}

	defer func() {
		res.EachRef(func(ref solver.CachedResult) error {
			go ref.Release(context.TODO())
			return nil
		})
	}()

	var exporterResponse map[string]string
	if exp := exp.Exporter; exp != nil {
		inp := exporter.Source{
			Metadata: res.Metadata,
		}
		if res := res.Ref; res != nil {
			workerRef, ok := res.Sys().(*worker.WorkerRef)
			if !ok {
				return nil, errors.Errorf("invalid reference: %T", res.Sys())
			}
			inp.Ref = workerRef.ImmutableRef
		}
		if res.Refs != nil {
			m := make(map[string]cache.ImmutableRef, len(res.Refs))
			for k, res := range res.Refs {
				if res == nil {
					m[k] = nil
				} else {
					workerRef, ok := res.Sys().(*worker.WorkerRef)
					if !ok {
						return nil, errors.Errorf("invalid reference: %T", res.Sys())
					}
					m[k] = workerRef.ImmutableRef
				}
			}
			inp.Refs = m
		}

		if err := inVertexContext(j.Context(ctx), exp.Name(), func(ctx context.Context) error {
			exporterResponse, err = exp.Export(ctx, inp)
			return err
		}); err != nil {
			return nil, err
		}
	}

	if e := exp.CacheExporter; e != nil {
		if err := inVertexContext(j.Context(ctx), "exporting cache", func(ctx context.Context) error {
			prepareDone := oneOffProgress(ctx, "preparing build cache for export")
			if err := res.EachRef(func(res solver.CachedResult) error {
				// all keys have same export chain so exporting others is not needed
				_, err := res.CacheKeys()[0].Exporter.ExportTo(ctx, e, solver.CacheExportOpt{
					Convert: workerRefConverter,
					Mode:    exp.CacheExportMode,
				})
				return err
			}); err != nil {
				return prepareDone(err)
			}
			prepareDone(nil)
			return e.Finalize(ctx)
		}); err != nil {
			return nil, err
		}
	}

	return &client.SolveResponse{
		ExporterResponse: exporterResponse,
	}, nil
}

func (s *Solver) Status(ctx context.Context, id string, statusChan chan *client.SolveStatus) error {
	j, err := s.solver.Get(id)
	if err != nil {
		return err
	}
	return j.Status(ctx, statusChan)
}

func defaultResolver(wc *worker.Controller) ResolveWorkerFunc {
	return func() (worker.Worker, error) {
		return wc.GetDefault()
	}
}

func oneOffProgress(ctx context.Context, id string) func(err error) error {
	pw, _, _ := progress.FromContext(ctx)
	now := time.Now()
	st := progress.Status{
		Started: &now,
	}
	pw.Write(id, st)
	return func(err error) error {
		// TODO: set error on status
		now := time.Now()
		st.Completed = &now
		pw.Write(id, st)
		pw.Close()
		return err
	}
}

func inVertexContext(ctx context.Context, name string, f func(ctx context.Context) error) error {
	v := client.Vertex{
		Digest: digest.FromBytes([]byte(identity.NewID())),
		Name:   name,
	}
	pw, _, ctx := progress.FromContext(ctx, progress.WithMetadata("vertex", v.Digest))
	notifyStarted(ctx, &v, false)
	defer pw.Close()
	err := f(ctx)
	notifyCompleted(ctx, &v, err, false)
	return err
}

func notifyStarted(ctx context.Context, v *client.Vertex, cached bool) {
	pw, _, _ := progress.FromContext(ctx)
	defer pw.Close()
	now := time.Now()
	v.Started = &now
	v.Completed = nil
	v.Cached = cached
	pw.Write(v.Digest.String(), *v)
}

func notifyCompleted(ctx context.Context, v *client.Vertex, err error, cached bool) {
	pw, _, _ := progress.FromContext(ctx)
	defer pw.Close()
	now := time.Now()
	if v.Started == nil {
		v.Started = &now
	}
	v.Completed = &now
	v.Cached = cached
	if err != nil {
		v.Error = err.Error()
	}
	pw.Write(v.Digest.String(), *v)
}
