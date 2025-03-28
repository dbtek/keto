// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package check_test

import (
	"context"
	"testing"

	"github.com/gofrs/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ory/keto/internal/check"
	"github.com/ory/keto/internal/driver"
	"github.com/ory/keto/internal/driver/config"
	"github.com/ory/keto/internal/namespace"
	"github.com/ory/keto/internal/relationtuple"
	"github.com/ory/keto/internal/x"
	"github.com/ory/keto/ketoapi"
)

type configProvider = config.Provider
type loggerProvider = x.LoggerProvider

// deps is defined to capture engine dependencies in a single struct
type deps struct {
	*relationtuple.ManagerWrapper // managerProvider
	configProvider
	loggerProvider
}

func newDepsProvider(t testing.TB, namespaces []*namespace.Namespace, pageOpts ...x.PaginationOptionSetter) *deps {
	reg := driver.NewSqliteTestRegistry(t, false)
	require.NoError(t, reg.Config(context.Background()).Set(config.KeyNamespaces, namespaces))
	mr := relationtuple.NewManagerWrapper(t, reg, pageOpts...)

	return &deps{
		ManagerWrapper: mr,
		configProvider: reg,
		loggerProvider: reg,
	}
}

func toUUID(s string) uuid.UUID {
	return uuid.NewV5(uuid.Nil, s)
}

func tupleFromString(t testing.TB, s string) *relationtuple.RelationTuple {
	rt, err := (&ketoapi.RelationTuple{}).FromString(s)
	require.NoError(t, err)
	result := &relationtuple.RelationTuple{
		Namespace: rt.Namespace,
		Object:    toUUID(rt.Object),
		Relation:  rt.Relation,
	}
	switch {
	case rt.SubjectID != nil:
		result.Subject = &relationtuple.SubjectID{ID: toUUID(*rt.SubjectID)}
	case rt.SubjectSet != nil:
		result.Subject = &relationtuple.SubjectSet{
			Namespace: rt.SubjectSet.Namespace,
			Object:    toUUID(rt.SubjectSet.Object),
			Relation:  rt.SubjectSet.Relation,
		}
	default:
		t.Fatal("invalid tuple")
	}
	return result
}

func TestEngine(t *testing.T) {
	ctx := context.Background()

	t.Run("respects max depth", func(t *testing.T) {
		// "user" has relation "access" through being an "owner" through being an "admin"
		// which requires at least 2 units of depth. If max-depth is 2 then we hit max-depth
		reg := newDepsProvider(t, []*namespace.Namespace{
			{Name: "test"},
		})

		// "user" has relation "access" through being an "owner" through being
		// an "admin" which requires at least 2 units of depth. If max-depth is
		// 2 then we hit max-depth
		insertFixtures(t, reg.RelationTupleManager(), []string{
			"test:object#admin@user",
			"test:object#owner@test:object#admin",
			"test:object#access@test:object#owner",
		})

		e := check.NewEngine(reg)

		userHasAccess := tupleFromString(t, "test:object#access@user")

		// global max-depth defaults to 5
		assert.Equal(t, reg.Config(ctx).MaxReadDepth(), 5)

		// req max-depth takes precedence, max-depth=2 is not enough
		res, err := e.CheckIsMember(ctx, userHasAccess, 2)
		require.NoError(t, err)
		assert.False(t, res)

		// req max-depth takes precedence, max-depth=3 is enough
		res, err = e.CheckIsMember(ctx, userHasAccess, 3)
		require.NoError(t, err)
		assert.True(t, res)

		// global max-depth takes precedence and max-depth=2 is not enough
		require.NoError(t, reg.Config(ctx).Set(config.KeyLimitMaxReadDepth, 2))
		res, err = e.CheckIsMember(ctx, userHasAccess, 3)
		require.NoError(t, err)
		assert.False(t, res)

		// global max-depth takes precedence and max-depth=3 is enough
		require.NoError(t, reg.Config(ctx).Set(config.KeyLimitMaxReadDepth, 3))
		res, err = e.CheckIsMember(ctx, userHasAccess, 0)
		require.NoError(t, err)
		assert.True(t, res)
	})

	t.Run("direct inclusion", func(t *testing.T) {
		reg := newDepsProvider(t, []*namespace.Namespace{{Name: "n"}, {Name: "u"}})
		tuples := []string{
			`n:o#r@subject_id`,
			`n:o#r@u:with_relation#r`,
			`n:o#r@u:empty_relation#`,
			`n:o#r@u:missing_relation`,
		}

		insertFixtures(t, reg.RelationTupleManager(), tuples)
		e := check.NewEngine(reg)

		cases := []struct {
			tuple string
		}{
			{tuple: "n:o#r@subject_id"},
			{tuple: "n:o#r@u:with_relation#r"},

			{tuple: "n:o#r@u:empty_relation"},
			{tuple: "n:o#r@u:empty_relation#"},

			{tuple: "n:o#r@u:missing_relation"},
			{tuple: "n:o#r@u:missing_relation#"},
		}

		for _, tc := range cases {
			t.Run("case="+tc.tuple, func(t *testing.T) {
				res, err := e.CheckIsMember(ctx, tupleFromString(t, tc.tuple), 0)
				require.NoError(t, err)
				assert.True(t, res)
			})
		}
	})

	t.Run("indirect inclusion level 1", func(t *testing.T) {
		// the set of users that are produces of "dust" have to remove it
		dust := uuid.Must(uuid.NewV4())
		sofaNamespace := "sofa"
		mark := relationtuple.SubjectID{ID: uuid.Must(uuid.NewV4())}
		cleaningRelation := relationtuple.RelationTuple{
			Namespace: sofaNamespace,
			Relation:  "have to remove",
			Object:    dust,
			Subject: &relationtuple.SubjectSet{
				Relation:  "producer",
				Object:    dust,
				Namespace: sofaNamespace,
			},
		}
		markProducesDust := relationtuple.RelationTuple{
			Namespace: sofaNamespace,
			Relation:  "producer",
			Object:    dust,
			Subject:   &mark,
		}

		reg := newDepsProvider(t, []*namespace.Namespace{
			{Name: sofaNamespace},
		})
		require.NoError(t, reg.RelationTupleManager().WriteRelationTuples(ctx, &cleaningRelation, &markProducesDust))

		e := check.NewEngine(reg)

		res, err := e.CheckIsMember(ctx, &relationtuple.RelationTuple{
			Relation:  cleaningRelation.Relation,
			Object:    dust,
			Subject:   &mark,
			Namespace: sofaNamespace,
		}, 0)
		require.NoError(t, err)
		assert.True(t, res)
	})

	t.Run("direct exclusion", func(t *testing.T) {
		user := &relationtuple.SubjectID{ID: uuid.Must(uuid.NewV4())}
		rel := relationtuple.RelationTuple{
			Relation:  "relation",
			Object:    uuid.Must(uuid.NewV4()),
			Namespace: t.Name(),
			Subject:   user,
		}

		reg := newDepsProvider(t, []*namespace.Namespace{{Name: rel.Namespace}})
		require.NoError(t, reg.RelationTupleManager().WriteRelationTuples(ctx, &rel))

		e := check.NewEngine(reg)

		res, err := e.CheckIsMember(ctx, &relationtuple.RelationTuple{
			Relation:  rel.Relation,
			Object:    rel.Object,
			Namespace: rel.Namespace,
			Subject:   &relationtuple.SubjectID{ID: uuid.Must(uuid.NewV4())},
		}, 0)
		require.NoError(t, err)
		assert.False(t, res)
	})

	t.Run("wrong object ID", func(t *testing.T) {
		object := uuid.Must(uuid.NewV4())
		access := relationtuple.RelationTuple{
			Relation: "access",
			Object:   object,
			Subject: &relationtuple.SubjectSet{
				Relation: "owner",
				Object:   object,
			},
		}
		user := relationtuple.RelationTuple{
			Relation: "owner",
			Object:   uuid.Must(uuid.NewV4()),
			Subject:  &relationtuple.SubjectID{ID: uuid.Must(uuid.NewV4())},
		}

		reg := newDepsProvider(t, []*namespace.Namespace{{Name: ""}})
		require.NoError(t, reg.RelationTupleManager().WriteRelationTuples(ctx, &access, &user))

		e := check.NewEngine(reg)

		res, err := e.CheckIsMember(ctx, &relationtuple.RelationTuple{
			Relation: access.Relation,
			Object:   object,
			Subject:  user.Subject,
		}, 0)
		require.NoError(t, err)
		assert.False(t, res)
	})

	t.Run("wrong relation name", func(t *testing.T) {
		diaryEntry := uuid.Must(uuid.NewV4())
		diaryNamespace := "diaries"
		// this would be a user-set rewrite
		readDiary := relationtuple.RelationTuple{
			Namespace: diaryNamespace,
			Relation:  "read",
			Object:    diaryEntry,
			Subject: &relationtuple.SubjectSet{
				Relation:  "author",
				Object:    diaryEntry,
				Namespace: diaryNamespace,
			},
		}
		user := relationtuple.RelationTuple{
			Namespace: diaryNamespace,
			Relation:  "not author",
			Object:    diaryEntry,
			Subject:   &relationtuple.SubjectID{ID: uuid.Must(uuid.NewV4())},
		}

		reg := newDepsProvider(t, []*namespace.Namespace{
			{Name: diaryNamespace},
		})
		require.NoError(t, reg.RelationTupleManager().WriteRelationTuples(ctx, &readDiary, &user))

		e := check.NewEngine(reg)

		res, err := e.CheckIsMember(ctx, &relationtuple.RelationTuple{
			Relation:  readDiary.Relation,
			Object:    diaryEntry,
			Namespace: diaryNamespace,
			Subject:   user.Subject,
		}, 0)
		require.NoError(t, err)
		assert.False(t, res)
	})

	t.Run("indirect inclusion level 2", func(t *testing.T) {
		object := uuid.Must(uuid.NewV4())
		someNamespace := "some_namespaces"
		user := relationtuple.SubjectID{ID: uuid.Must(uuid.NewV4())}
		organization := uuid.Must(uuid.NewV4())
		orgNamespace := "organizations"

		ownerUserSet := relationtuple.SubjectSet{
			Namespace: someNamespace,
			Relation:  "owner",
			Object:    object,
		}
		orgMembers := relationtuple.SubjectSet{
			Namespace: orgNamespace,
			Relation:  "member",
			Object:    organization,
		}

		writeRel := relationtuple.RelationTuple{
			Namespace: someNamespace,
			Relation:  "write",
			Object:    object,
			Subject:   &ownerUserSet,
		}
		orgOwnerRel := relationtuple.RelationTuple{
			Namespace: someNamespace,
			Relation:  ownerUserSet.Relation,
			Object:    object,
			Subject:   &orgMembers,
		}
		userMembershipRel := relationtuple.RelationTuple{
			Namespace: orgNamespace,
			Relation:  orgMembers.Relation,
			Object:    organization,
			Subject:   &user,
		}

		reg := newDepsProvider(t, []*namespace.Namespace{
			{Name: someNamespace},
			{Name: orgNamespace},
		})
		require.NoError(t, reg.RelationTupleManager().WriteRelationTuples(ctx, &writeRel, &orgOwnerRel, &userMembershipRel))

		e := check.NewEngine(reg)

		// user can write object
		res, err := e.CheckIsMember(ctx, &relationtuple.RelationTuple{
			Namespace: someNamespace,
			Relation:  writeRel.Relation,
			Object:    object,
			Subject:   &user,
		}, 0)
		require.NoError(t, err)
		assert.True(t, res)

		// user is member of the organization
		res, err = e.CheckIsMember(ctx, &relationtuple.RelationTuple{
			Namespace: orgNamespace,
			Relation:  orgMembers.Relation,
			Object:    organization,
			Subject:   &user,
		}, 0)
		require.NoError(t, err)
		assert.True(t, res)
	})

	t.Run("rejects transitive relation", func(t *testing.T) {
		// (file) <--parent-- (directory) <--access-- [user]
		//
		// note the missing access relation from "users who have access to directory also have access to files inside of the directory"
		// as we don't know how to interpret the "parent" relation, there would have to be a userset rewrite to allow access
		// to files when you have access to the parent

		file := uuid.Must(uuid.NewV4())
		directory := uuid.Must(uuid.NewV4())
		user := relationtuple.SubjectID{ID: uuid.Must(uuid.NewV4())}

		parent := relationtuple.RelationTuple{
			Relation: "parent",
			Object:   file,
			Subject: &relationtuple.SubjectSet{ // <- this is only an object, but this is allowed as a userset can have the "..." relation which means any relation
				Object: directory,
			},
		}
		directoryAccess := relationtuple.RelationTuple{
			Relation: "access",
			Object:   directory,
			Subject:  &user,
		}

		reg := newDepsProvider(t, []*namespace.Namespace{
			{Name: "2"},
		})
		require.NoError(t, reg.RelationTupleManager().WriteRelationTuples(ctx, &parent, &directoryAccess))

		e := check.NewEngine(reg)

		res, err := e.CheckIsMember(ctx, &relationtuple.RelationTuple{
			Relation: directoryAccess.Relation,
			Object:   file,
			Subject:  &user,
		}, 0)
		require.NoError(t, err)
		assert.False(t, res)
	})

	t.Run("case=subject id next to subject set", func(t *testing.T) {
		namesp, obj, org, directOwner, indirectOwner, ownerRel, memberRel := "39231", uuid.Must(uuid.NewV4()), uuid.Must(uuid.NewV4()), uuid.Must(uuid.NewV4()), uuid.Must(uuid.NewV4()), "owner", "member"

		reg := newDepsProvider(t, []*namespace.Namespace{
			{Name: namesp},
		})
		require.NoError(t, reg.RelationTupleManager().WriteRelationTuples(
			ctx,
			&relationtuple.RelationTuple{
				Namespace: namesp,
				Object:    obj,
				Relation:  ownerRel,
				Subject:   &relationtuple.SubjectID{ID: directOwner},
			},
			&relationtuple.RelationTuple{
				Namespace: namesp,
				Object:    obj,
				Relation:  ownerRel,
				Subject: &relationtuple.SubjectSet{
					Namespace: namesp,
					Object:    org,
					Relation:  memberRel,
				},
			},
			&relationtuple.RelationTuple{
				Namespace: namesp,
				Object:    org,
				Relation:  memberRel,
				Subject:   &relationtuple.SubjectID{ID: indirectOwner},
			},
		))

		e := check.NewEngine(reg)

		res, err := e.CheckIsMember(ctx, &relationtuple.RelationTuple{
			Namespace: namesp,
			Object:    obj,
			Relation:  ownerRel,
			Subject:   &relationtuple.SubjectID{ID: directOwner},
		}, 0)
		require.NoError(t, err)
		assert.True(t, res)

		res, err = e.CheckIsMember(ctx, &relationtuple.RelationTuple{
			Namespace: namesp,
			Object:    obj,
			Relation:  ownerRel,
			Subject:   &relationtuple.SubjectID{ID: indirectOwner},
		}, 0)
		require.NoError(t, err)
		assert.True(t, res)
	})

	t.Run("case=wide tuple graph", func(t *testing.T) {
		namesp, obj, access, member, users, orgs := "9234", uuid.Must(uuid.NewV4()), "access", "member", x.UUIDs(4), x.UUIDs(2)

		reg := newDepsProvider(t, []*namespace.Namespace{{Name: namesp}})

		for _, org := range orgs {
			require.NoError(t, reg.RelationTupleManager().WriteRelationTuples(ctx, &relationtuple.RelationTuple{
				Namespace: namesp,
				Object:    obj,
				Relation:  access,
				Subject: &relationtuple.SubjectSet{
					Namespace: namesp,
					Object:    org,
					Relation:  member,
				},
			}))
		}

		for i, user := range users {
			require.NoError(t, reg.RelationTupleManager().WriteRelationTuples(ctx, &relationtuple.RelationTuple{
				Namespace: namesp,
				Object:    orgs[i%len(orgs)],
				Relation:  member,
				Subject:   &relationtuple.SubjectID{ID: user},
			}))
		}

		e := check.NewEngine(reg)

		for _, user := range users {
			req := &relationtuple.RelationTuple{
				Namespace: namesp,
				Object:    obj,
				Relation:  access,
				Subject:   &relationtuple.SubjectID{ID: user},
			}
			allowed, err := e.CheckIsMember(ctx, req, 0)
			require.NoError(t, err)
			assert.Truef(t, allowed, "%+v", req)
		}
	})

	t.Run("case=circular tuples", func(t *testing.T) {
		sendlingerTor, odeonsplatz, centralStation, connected, namesp := uuid.NewV5(uuid.Nil, "Sendlinger Tor"), uuid.NewV5(uuid.Nil, "Odeonsplatz"), uuid.NewV5(uuid.Nil, "Central Station"), "connected", "7743"

		reg := newDepsProvider(t, []*namespace.Namespace{{Name: namesp}})

		require.NoError(t, reg.RelationTupleManager().WriteRelationTuples(ctx, []*relationtuple.RelationTuple{
			{
				Namespace: namesp,
				Object:    sendlingerTor,
				Relation:  connected,
				Subject: &relationtuple.SubjectSet{
					Namespace: namesp,
					Object:    odeonsplatz,
					Relation:  connected,
				},
			},
			{
				Namespace: namesp,
				Object:    odeonsplatz,
				Relation:  connected,
				Subject: &relationtuple.SubjectSet{
					Namespace: namesp,
					Object:    centralStation,
					Relation:  connected,
				},
			},
			{
				Namespace: namesp,
				Object:    centralStation,
				Relation:  connected,
				Subject: &relationtuple.SubjectSet{
					Namespace: namesp,
					Object:    sendlingerTor,
					Relation:  connected,
				},
			},
		}...))

		e := check.NewEngine(reg)

		stations := []uuid.UUID{sendlingerTor, odeonsplatz, centralStation}
		res, err := e.CheckIsMember(ctx, &relationtuple.RelationTuple{
			Namespace: namesp,
			Object:    stations[0],
			Relation:  connected,
			Subject: &relationtuple.SubjectID{
				ID: stations[2],
			},
		}, 0)
		require.NoError(t, err)
		assert.False(t, res)
	})
}
