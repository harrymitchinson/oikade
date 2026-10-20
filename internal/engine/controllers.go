package engine

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
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

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
)

// Loader keeps the engine's Rules in sync with the API.
type Loader struct {
	client.Client
	Engine *Engine
}

// Reconcile compiles and loads one Rule. Invalid ones are unloaded; the operator reports why.
func (l *Loader) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var a homev1alpha1.Rule
	if err := l.Get(ctx, req.NamespacedName, &a); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		l.Engine.Remove(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	c, err := Compile(&a)
	if err != nil {
		logf.FromContext(ctx).Info("Unloaded invalid Rule", "rule", req.NamespacedName, "error", err.Error())
		l.Engine.Remove(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	l.Engine.Load(c)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the loader.
func (l *Loader) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Status updates must not reload (and so cancel) a running Rule.
		For(&homev1alpha1.Rule{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("rule-loader").
		Complete(l)
}

// Triggered records when Rules last started and writes status.lastTriggered, at most once
// per Interval per Rule.
type Triggered struct {
	client.Client
	Interval time.Duration

	mu     sync.Mutex
	latest map[types.NamespacedName]time.Time
	events chan event.GenericEvent
}

// Record is an Engine.OnStart hook.
func (t *Triggered) Record(key types.NamespacedName, at time.Time) {
	t.mu.Lock()
	t.latest[key] = at
	t.mu.Unlock()
	obj := &homev1alpha1.Rule{}
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
	var a homev1alpha1.Rule
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
		Named("rule-triggered").
		WatchesRawSource(source.Channel(t.events, &handler.EnqueueRequestForObject{})).
		Complete(t)
}

// Scheduled writes what the engine reports about Rules' at and sun triggers: status.nextFire,
// status.lastFire and the Scheduled condition.
type Scheduled struct {
	client.Client

	mu     sync.Mutex
	latest map[types.NamespacedName]Schedule
	events chan event.GenericEvent
}

// Record is an Engine.OnSchedule hook. A zero LastFire keeps the one already recorded. It never
// drops a write: when the queue is full, it queues from a goroutine.
func (s *Scheduled) Record(key types.NamespacedName, sched Schedule) {
	s.mu.Lock()
	if prev, ok := s.latest[key]; ok && sched.LastFire.IsZero() && !sched.Cleared && !prev.Cleared {
		sched.LastFire = prev.LastFire
	}
	s.latest[key] = sched
	s.mu.Unlock()
	obj := &homev1alpha1.Rule{}
	obj.Name, obj.Namespace = key.Name, key.Namespace
	ev := event.GenericEvent{Object: obj}
	select {
	case s.events <- ev:
	default:
		go func() { s.events <- ev }()
	}
}

// Reconcile writes the schedule. It patches with optimistic locking, because the operator writes
// the Ready condition into the same list.
func (s *Scheduled) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	s.mu.Lock()
	sched, ok := s.latest[req.NamespacedName]
	s.mu.Unlock()
	if !ok {
		return ctrl.Result{}, nil
	}
	var a homev1alpha1.Rule
	if err := s.Get(ctx, req.NamespacedName, &a); err != nil {
		if client.IgnoreNotFound(err) == nil {
			s.forget(req.NamespacedName, sched)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := a.DeepCopy()
	a.Status.NextFire = metav1.Time{}
	if sched.Cleared {
		meta.RemoveStatusCondition(&a.Status.Conditions, homev1alpha1.ConditionScheduled)
		if !equality.Semantic.DeepEqual(orig.Status, a.Status) {
			if err := s.Status().Patch(ctx, &a, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
		}
		s.forget(req.NamespacedName, sched)
		return ctrl.Result{}, nil
	}
	if !sched.Next.IsZero() {
		a.Status.NextFire = metav1.NewTime(sched.Next.Truncate(time.Second))
	}
	if last := sched.LastFire.Truncate(time.Second); last.After(a.Status.LastFire.Time) {
		a.Status.LastFire = metav1.NewTime(last)
	}
	cond := metav1.Condition{
		Type: homev1alpha1.ConditionScheduled, Status: metav1.ConditionTrue, Reason: "Scheduled",
		ObservedGeneration: sched.Generation,
	}
	if sched.Reason == ReasonNoLocation {
		cond.Status, cond.Reason = metav1.ConditionFalse, ReasonNoLocation
		cond.Message = "Sun triggers need the runtime's --latitude and --longitude"
	}
	meta.SetStatusCondition(&a.Status.Conditions, cond)
	if equality.Semantic.DeepEqual(orig.Status, a.Status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, s.Status().Patch(ctx, &a, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

// forget drops a Rule's schedule once it's cleared or the Rule is gone, unless a newer one came.
func (s *Scheduled) forget(key types.NamespacedName, sched Schedule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.latest[key]; ok && cur == sched {
		delete(s.latest, key)
	}
}

// SetupWithManager registers the status writer.
func (s *Scheduled) SetupWithManager(mgr ctrl.Manager) error {
	s.latest = map[types.NamespacedName]Schedule{}
	s.events = make(chan event.GenericEvent, 256)
	return ctrl.NewControllerManagedBy(mgr).
		Named("rule-scheduled").
		WatchesRawSource(source.Channel(s.events, &handler.EnqueueRequestForObject{})).
		Complete(s)
}
