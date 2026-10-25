package engine

import (
	"context"
	"slices"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		l.Engine.Unload(req.NamespacedName)
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
// status.lastFire and the Scheduled condition. For a hold, it writes status.active and
// status.nextChange, and the Scheduled condition when the hold has sun times.
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
	a.Status.Active, a.Status.NextChange = false, metav1.Time{}
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
	next := metav1.Time{}
	if !sched.Next.IsZero() {
		next = metav1.NewTime(sched.Next.Truncate(time.Second))
	}
	if sched.Hold {
		a.Status.Active, a.Status.NextChange = sched.Active, next
	} else {
		a.Status.NextFire = next
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
		cond.Message = "Sun triggers and sun windows need the runtime's --latitude and --longitude"
	}
	if sched.Hold && sched.Reason == "" {
		// A hold only reports Scheduled when its sun times can't be computed.
		meta.RemoveStatusCondition(&a.Status.Conditions, homev1alpha1.ConditionScheduled)
	} else {
		meta.SetStatusCondition(&a.Status.Conditions, cond)
	}
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

// latest keeps the latest value reported per object and queues a reconcile for it. It never
// drops a write: when the queue is full, it queues from a goroutine.
type latest[T any] struct {
	mu     sync.Mutex
	values map[types.NamespacedName]T
	events chan event.GenericEvent
}

func (l *latest[T]) init() {
	l.values = map[types.NamespacedName]T{}
	l.events = make(chan event.GenericEvent, 256)
}

func (l *latest[T]) record(key types.NamespacedName, v T, obj client.Object) {
	l.mu.Lock()
	l.values[key] = v
	l.mu.Unlock()
	obj.SetName(key.Name)
	obj.SetNamespace(key.Namespace)
	ev := event.GenericEvent{Object: obj}
	select {
	case l.events <- ev:
	default:
		go func() { l.events <- ev }()
	}
}

// forget drops the value kept for an object that's gone. A newer report for one of the same
// name, created again, records it again.
func (l *latest[T]) forget(key types.NamespacedName) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.values, key)
}

func (l *latest[T]) get(key types.NamespacedName) (T, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.values[key]
	return v, ok
}

// ControlStatus writes Device.status.control from what the engine reports.
type ControlStatus struct {
	client.Client
	latest latest[[]homev1alpha1.CapabilityControlStatus]
}

// Record is an Engine.OnControl hook.
func (c *ControlStatus) Record(dev types.NamespacedName, control []homev1alpha1.CapabilityControlStatus) {
	c.latest.record(dev, control, &homev1alpha1.Device{})
}

// Reconcile writes the Device's control status. The runtime is its only writer.
func (c *ControlStatus) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	control, ok := c.latest.get(req.NamespacedName)
	if !ok {
		return ctrl.Result{}, nil
	}
	var d homev1alpha1.Device
	if err := c.Get(ctx, req.NamespacedName, &d); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if equality.Semantic.DeepEqual(d.Status.Control, control) {
		return ctrl.Result{}, nil
	}
	orig := d.DeepCopy()
	d.Status.Control = control
	return ctrl.Result{}, c.Status().Patch(ctx, &d, client.MergeFrom(orig))
}

// SetupWithManager registers the status writer.
func (c *ControlStatus) SetupWithManager(mgr ctrl.Manager) error {
	c.latest.init()
	return ctrl.NewControllerManagedBy(mgr).
		Named("device-control").
		WatchesRawSource(source.Channel(c.latest.events, &handler.EnqueueRequestForObject{})).
		Complete(c)
}

// Skipped writes Rule.status.skipped from what the engine reports.
type Skipped struct {
	client.Client
	latest latest[[]string]
}

// Record is an Engine.OnSkipped hook.
func (s *Skipped) Record(key types.NamespacedName, skipped []string) {
	s.latest.record(key, skipped, &homev1alpha1.Rule{})
}

// Reconcile writes the Rule's skipped Room members.
func (s *Skipped) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	skipped, ok := s.latest.get(req.NamespacedName)
	if !ok {
		return ctrl.Result{}, nil
	}
	var r homev1alpha1.Rule
	if err := s.Get(ctx, req.NamespacedName, &r); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if slices.Equal(r.Status.Skipped, skipped) {
		return ctrl.Result{}, nil
	}
	orig := r.DeepCopy()
	r.Status.Skipped = skipped
	return ctrl.Result{}, s.Status().Patch(ctx, &r, client.MergeFrom(orig))
}

// SetupWithManager registers the status writer.
func (s *Skipped) SetupWithManager(mgr ctrl.Manager) error {
	s.latest.init()
	return ctrl.NewControllerManagedBy(mgr).
		Named("rule-skipped").
		WatchesRawSource(source.Channel(s.latest.events, &handler.EnqueueRequestForObject{})).
		Complete(s)
}

// Budgets writes Rule.status.today and the Planned condition from what the engine reports about
// budget holds.
type Budgets struct {
	client.Client
	latest latest[BudgetReport]
}

// Record is an Engine.OnBudget hook.
func (b *Budgets) Record(key types.NamespacedName, r BudgetReport) {
	b.latest.record(key, r, &homev1alpha1.Rule{})
}

// Reconcile writes the Rule's plan. It patches with optimistic locking, because the operator
// writes the Ready condition into the same list.
func (b *Budgets) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r, ok := b.latest.get(req.NamespacedName)
	if !ok {
		return ctrl.Result{}, nil
	}
	var a homev1alpha1.Rule
	if err := b.Get(ctx, req.NamespacedName, &a); err != nil {
		if apierrors.IsNotFound(err) {
			b.latest.forget(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := a.DeepCopy()
	if r.Cleared {
		a.Status.Today = nil
		meta.RemoveStatusCondition(&a.Status.Conditions, homev1alpha1.ConditionPlanned)
	} else {
		if r.Today != nil {
			// A copy, since decoding the patch's response writes into it.
			a.Status.Today = r.Today.DeepCopy()
		}
		meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{
			Type: homev1alpha1.ConditionPlanned, Status: r.Planned, Reason: r.Reason, Message: r.Message,
			ObservedGeneration: r.Generation,
		})
	}
	if equality.Semantic.DeepEqual(orig.Status, a.Status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, b.Status().Patch(ctx, &a, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

// SetupWithManager registers the status writer.
func (b *Budgets) SetupWithManager(mgr ctrl.Manager) error {
	b.latest.init()
	return ctrl.NewControllerManagedBy(mgr).
		Named("rule-budget").
		WatchesRawSource(source.Channel(b.latest.events, &handler.EnqueueRequestForObject{})).
		Complete(b)
}
