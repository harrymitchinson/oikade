/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package engine

import (
	"context"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	homev1alpha1 "github.com/harry/oikade/api/home/v1alpha1"
)

// Loader keeps the engine's Automations in sync with the API.
type Loader struct {
	client.Client
	Engine *Engine
}

// +kubebuilder:rbac:groups=home.oikade.dev,resources=automations,verbs=get;list;watch

// Reconcile compiles and loads one Automation. Invalid ones are unloaded; the operator reports why.
func (l *Loader) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var a homev1alpha1.Automation
	if err := l.Get(ctx, req.NamespacedName, &a); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		l.Engine.Remove(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	c, err := Compile(&a)
	if err != nil {
		logf.FromContext(ctx).Info("Unloaded invalid Automation", "automation", req.NamespacedName, "error", err.Error())
		l.Engine.Remove(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	l.Engine.Load(c)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the loader.
func (l *Loader) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Status updates must not reload (and so cancel) a running Automation.
		For(&homev1alpha1.Automation{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("automation-loader").
		Complete(l)
}

// Triggered records when Automations last started and writes status.lastTriggered, at most once
// per Interval per Automation.
type Triggered struct {
	client.Client
	Interval time.Duration

	mu     sync.Mutex
	latest map[types.NamespacedName]time.Time
	events chan event.GenericEvent
}

// +kubebuilder:rbac:groups=home.oikade.dev,resources=automations/status,verbs=get;update;patch

// Record is an Engine.OnStart hook.
func (t *Triggered) Record(key types.NamespacedName, at time.Time) {
	t.mu.Lock()
	t.latest[key] = at
	t.mu.Unlock()
	obj := &homev1alpha1.Automation{}
	obj.Name, obj.Namespace = key.Name, key.Namespace
	select {
	case t.events <- event.GenericEvent{Object: obj}:
	default:
	}
}

// Reconcile writes lastTriggered.
func (t *Triggered) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	t.mu.Lock()
	at, ok := t.latest[req.NamespacedName]
	t.mu.Unlock()
	if !ok {
		return ctrl.Result{}, nil
	}
	var a homev1alpha1.Automation
	if err := t.Get(ctx, req.NamespacedName, &a); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	written := a.Status.LastTriggered.Time
	if !at.Truncate(time.Second).After(written) {
		return ctrl.Result{}, nil
	}
	if wait := t.Interval - time.Since(written); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	orig := a.DeepCopy()
	a.Status.LastTriggered = metav1.NewTime(at.Truncate(time.Second))
	return ctrl.Result{}, t.Status().Patch(ctx, &a, client.MergeFrom(orig))
}

// SetupWithManager registers the status writer.
func (t *Triggered) SetupWithManager(mgr ctrl.Manager) error {
	t.latest = map[types.NamespacedName]time.Time{}
	t.events = make(chan event.GenericEvent, 256)
	return ctrl.NewControllerManagedBy(mgr).
		Named("automation-triggered").
		WatchesRawSource(source.Channel(t.events, &handler.EnqueueRequestForObject{})).
		Complete(t)
}
