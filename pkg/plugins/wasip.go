//go:build linux || darwin

package plugins

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/http-wasm/http-wasm-host-go/handler"
	"github.com/stealthrocket/wasi-go/imports"
	wazergo_wasip1 "github.com/stealthrocket/wasi-go/imports/wasi_snapshot_preview1"
	"github.com/stealthrocket/wazergo"
	"github.com/tetratelabs/wazero"
	wazero_wasip1 "github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

type ContextApplier func(ctx context.Context) context.Context

// wasiInstanceKey is the context key under which wazergo looks up the WASI host
// module instance that serves a host call (see wazergo.WithModuleInstance).
type wasiInstanceKey = *wazergo.ModuleInstance[*wazergo_wasip1.Module]

// InstantiateHost instantiates the Host module according to the guest requirements (for now only SocketExtensions).
func InstantiateHost(ctx context.Context, runtime wazero.Runtime, mod wazero.CompiledModule, settings Settings) (ContextApplier, error) {
	if extension := imports.DetectSocketsExtension(mod); extension != nil {
		ctx, sys, err := newWASIBuilder(mod, settings).Instantiate(ctx, runtime)
		if err != nil {
			return nil, err
		}

		inst, err := wazergo.Instantiate(ctx, runtime, wazergo_wasip1.NewHostModule(*extension), wazergo_wasip1.WithWASI(sys))
		if err != nil {
			return nil, fmt.Errorf("wazergo instantiation: %w", err)
		}

		return func(ctx context.Context) context.Context {
			return wazergo.WithModuleInstance(ctx, inst)
		}, nil
	}

	_, err := wazero_wasip1.Instantiate(ctx, runtime)
	if err != nil {
		return nil, fmt.Errorf("wazero instantiation: %w", err)
	}

	return func(ctx context.Context) context.Context {
		return ctx
	}, nil
}

// GuestContext gives each instance of a guest that uses the sockets extension
// its own WASI system. It returns nil for other guests.
//
// The WASI host module keeps scratch buffers for every I/O call, and its system
// keeps the file descriptor table and the poll state; none of them is safe for
// concurrent use. The middleware runs one guest instance per in-flight request,
// so with a single WASI instance two concurrent requests overwrite each other's
// I/O vectors: a guest gets the byte count of another call ("invalid return
// from write") or bytes written into its memory, and its runtime crashes.
func GuestContext(mod wazero.CompiledModule, settings Settings) handler.NewGuestContext {
	if imports.DetectSocketsExtension(mod) == nil {
		return nil
	}

	return func(ctx context.Context) (func(context.Context) context.Context, func(context.Context) error, error) {
		// A runtime of its own holds the WASI host module instance of this guest.
		// The guest itself runs in the middleware runtime, where host calls
		// resolve the WASI instance from the call context.
		rt := wazero.NewRuntime(ctx)

		guestCtx, sys, err := newWASIBuilder(mod, settings).Instantiate(ctx, rt)
		if err != nil {
			_ = rt.Close(ctx)
			return nil, nil, fmt.Errorf("instantiating guest WASI: %w", err)
		}

		inst := guestCtx.Value(wasiInstanceKey(nil))
		if inst == nil {
			_ = sys.Close(ctx)
			_ = rt.Close(ctx)
			return nil, nil, errors.New("instantiating guest WASI: no module instance")
		}

		decorate := func(ctx context.Context) context.Context {
			return context.WithValue(ctx, wasiInstanceKey(nil), inst)
		}

		// The runtime does not own the WASI system: close both, so that the
		// sockets of the guest are closed with it.
		closeState := func(ctx context.Context) error {
			return errors.Join(sys.Close(ctx), rt.Close(ctx))
		}

		return decorate, closeState, nil
	}
}

func newWASIBuilder(mod wazero.CompiledModule, settings Settings) *imports.Builder {
	envs := []string{}
	for _, env := range settings.Envs {
		envs = append(envs, fmt.Sprintf("%s=%s", env, os.Getenv(env)))
	}

	builder := imports.NewBuilder().WithSocketsExtension("auto", mod)
	if len(envs) > 0 {
		builder.WithEnv(envs...)
	}

	if len(settings.Mounts) > 0 {
		builder.WithDirs(settings.Mounts...)
	}

	return builder
}
