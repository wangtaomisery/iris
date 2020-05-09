package sessions

import (
	"net/http"
	"time"

	"github.com/kataras/iris/v12/context"
)

func init() {
	context.SetHandlerName("iris/sessions.*Handler", "iris.session")
}

// A Sessions manager should be responsible to Start a sesion, based
// on a Context, which should return
// a compatible Session interface, type. If the external session manager
// doesn't qualifies, then the user should code the rest of the functions with empty implementation.
//
// Sessions should be responsible to Destroy a session based
// on the Context.
type Sessions struct {
	config   Config
	provider *provider

	handlerCookieOpts []context.CookieOption // see `Handler`.
}

// New returns a new fast, feature-rich sessions manager
// it can be adapted to an iris station
func New(cfg Config) *Sessions {
	return &Sessions{
		config:   cfg.Validate(),
		provider: newProvider(),
	}
}

// UseDatabase adds a session database to the manager's provider,
// a session db doesn't have write access
func (s *Sessions) UseDatabase(db Database) {
	s.provider.RegisterDatabase(db)
}

// GetCookieOptions returns any cookie options registered for the `Handler` method.
func (s *Sessions) GetCookieOptions() []context.CookieOption {
	return s.handlerCookieOpts
}

// updateCookie gains the ability of updating the session browser cookie to any method which wants to update it
func (s *Sessions) updateCookie(ctx context.Context, sid string, expires time.Duration, options ...context.CookieOption) {
	cookie := &http.Cookie{}

	// The RFC makes no mention of encoding url value, so here I think to encode both sessionid key and the value using the safe(to put and to use as cookie) url-encoding
	cookie.Name = s.config.Cookie
	cookie.Value = sid
	cookie.Path = "/"
	cookie.HttpOnly = true

	// MaxAge=0 means no 'Max-Age' attribute specified.
	// MaxAge<0 means delete cookie now, equivalently 'Max-Age: 0'
	// MaxAge>0 means Max-Age attribute present and given in seconds
	if expires >= 0 {
		if expires == 0 { // unlimited life
			cookie.Expires = context.CookieExpireUnlimited
		} else { // > 0
			cookie.Expires = time.Now().Add(expires)
		}
		cookie.MaxAge = int(time.Until(cookie.Expires).Seconds())
	}

	ctx.UpsertCookie(cookie, options...)
}

// Start creates or retrieves an existing session for the particular request.
// Note that `Start` method will not respect configuration's `AllowReclaim`, `DisableSubdomainPersistence`, `CookieSecureTLS`,
// and `Encoding` settings.
// Register sessions as a middleware through the `Handler` method instead,
// which provides automatic resolution of a *sessions.Session input argument
// on MVC and APIContainer as well.
//
// NOTE: Use `app.Use(sess.Handler())` instead, avoid using `Start` manually.
func (s *Sessions) Start(ctx context.Context, cookieOptions ...context.CookieOption) *Session {
	cookieValue := ctx.GetCookie(s.config.Cookie, cookieOptions...)

	if cookieValue == "" { // cookie doesn't exist, let's generate a session and set a cookie.
		sid := s.config.SessionIDGenerator(ctx)

		sess := s.provider.Init(s, sid, s.config.Expires)
		sess.isNew = s.provider.db.Len(sid) == 0

		s.updateCookie(ctx, sid, s.config.Expires, cookieOptions...)

		return sess
	}

	return s.provider.Read(s, cookieValue, s.config.Expires)
}

const sessionContextKey = "iris.session"

// Handler returns a sessions middleware to register on application routes.
// To return the request's Session call the `Get(ctx)` package-level function.
//
// Call `Handler()` once per sessions manager.
func (s *Sessions) Handler(cookieOptions ...context.CookieOption) context.Handler {
	s.handlerCookieOpts = cookieOptions

	var requestOptions []context.CookieOption
	if s.config.AllowReclaim {
		requestOptions = append(requestOptions, context.CookieAllowReclaim(s.config.Cookie))
	}
	if !s.config.DisableSubdomainPersistence {
		requestOptions = append(requestOptions, context.CookieAllowSubdomains(s.config.Cookie))
	}
	if s.config.CookieSecureTLS {
		requestOptions = append(requestOptions, context.CookieSecure)
	}
	if s.config.Encoding != nil {
		requestOptions = append(requestOptions, context.CookieEncoding(s.config.Encoding, s.config.Cookie))
	}

	return func(ctx context.Context) {
		ctx.AddCookieOptions(requestOptions...) // request life-cycle options.

		session := s.Start(ctx, cookieOptions...) // this cookie's end-developer's custom options.

		ctx.Values().Set(sessionContextKey, session)
		ctx.Next()
	}
}

// Get returns a *Session from the same request life cycle,
// can be used inside a chain of handlers of a route.
//
// The `Sessions.Start` should be called previously,
// e.g. register the `Sessions.Handler` as middleware.
// Then call `Get` package-level function as many times as you want.
// Note: It will return nil if the session got destroyed by the same request.
// If you need to destroy and start a new session in the same request you need to call
// sessions manager's `Start` method after Destroy.
func Get(ctx context.Context) *Session {
	if v := ctx.Values().Get(sessionContextKey); v != nil {
		if sess, ok := v.(*Session); ok {
			return sess
		}
	}

	// ctx.Application().Logger().Debugf("Sessions: Get: no session found, prior Destroy(ctx) calls in the same request should follow with a Start(ctx) call too")
	return nil
}

// StartWithPath same as `Start` but it explicitly accepts the cookie path option.
func (s *Sessions) StartWithPath(ctx context.Context, path string) *Session {
	return s.Start(ctx, context.CookiePath(path))
}

// ShiftExpiration move the expire date of a session to a new date
// by using session default timeout configuration.
// It will return `ErrNotImplemented` if a database is used and it does not support this feature, yet.
func (s *Sessions) ShiftExpiration(ctx context.Context, cookieOptions ...context.CookieOption) error {
	return s.UpdateExpiration(ctx, s.config.Expires, cookieOptions...)
}

// UpdateExpiration change expire date of a session to a new date
// by using timeout value passed by `expires` receiver.
// It will return `ErrNotFound` when trying to update expiration on a non-existence or not valid session entry.
// It will return `ErrNotImplemented` if a database is used and it does not support this feature, yet.
func (s *Sessions) UpdateExpiration(ctx context.Context, expires time.Duration, cookieOptions ...context.CookieOption) error {
	cookieValue := ctx.GetCookie(s.config.Cookie)
	if cookieValue == "" {
		return ErrNotFound
	}

	// we should also allow it to expire when the browser closed
	err := s.provider.UpdateExpiration(cookieValue, expires)
	if err == nil || expires == -1 {
		s.updateCookie(ctx, cookieValue, expires, cookieOptions...)
	}

	return err
}

// DestroyListener is the form of a destroy listener.
// Look `OnDestroy` for more.
type DestroyListener func(sid string)

// OnDestroy registers one or more destroy listeners.
// A destroy listener is fired when a session has been removed entirely from the server (the entry) and client-side (the cookie).
// Note that if a destroy listener is blocking, then the session manager will delay respectfully,
// use a goroutine inside the listener to avoid that behavior.
func (s *Sessions) OnDestroy(listeners ...DestroyListener) {
	for _, ln := range listeners {
		s.provider.registerDestroyListener(ln)
	}
}

// Destroy removes the session data, the associated cookie
// and the Context's session value.
// Next calls of `sessions.Get` will occur to a nil Session,
// use `Sessions#Start` method for renewal
// or use the Session's Destroy method which does keep the session entry with its values cleared.
func (s *Sessions) Destroy(ctx context.Context) {
	cookieValue := ctx.GetCookie(s.config.Cookie)
	if cookieValue == "" { // nothing to destroy
		return
	}

	ctx.Values().Remove(sessionContextKey)

	ctx.RemoveCookie(s.config.Cookie)
	s.provider.Destroy(cookieValue)
}

// DestroyByID removes the session entry
// from the server-side memory (and database if registered).
// Client's session cookie will still exist but it will be reseted on the next request.
//
// It's safe to use it even if you are not sure if a session with that id exists.
//
// Note: the sid should be the original one (i.e: fetched by a store )
// it's not decoded.
func (s *Sessions) DestroyByID(sid string) {
	s.provider.Destroy(sid)
}

// DestroyAll removes all sessions
// from the server-side memory (and database if registered).
// Client's session cookie will still exist but it will be reseted on the next request.
func (s *Sessions) DestroyAll() {
	s.provider.DestroyAll()
}
