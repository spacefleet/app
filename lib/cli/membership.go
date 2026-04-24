package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/user"
	"github.com/redis/go-redis/v9"
)

// membershipCacheTTL intentionally trades a small staleness window for
// fewer Clerk API calls. Revocations take up to this long to propagate to
// CLI traffic — acceptable given revocation is rare and the blast radius is
// one user.
const membershipCacheTTL = 5 * time.Minute

// UserCanAccessOrg returns true if userID is a member of the Clerk
// organization identified by orgSlug. The result is cached in Redis under
// `cli:member:{userID}:{orgSlug}` for 5 minutes (shared across replicas).
func (s *Service) UserCanAccessOrg(ctx context.Context, userID, orgSlug string) (bool, error) {
	key := membershipCacheKey(userID, orgSlug)
	if v, err := s.redis.Get(ctx, key).Result(); err == nil {
		return v == "1", nil
	} else if !errors.Is(err, redis.Nil) {
		// Redis down → fall through to the source of truth rather than
		// denying access on infra hiccups.
		allowed, rerr := s.resolveMembership(ctx, userID, orgSlug)
		return allowed, rerr
	}

	allowed, err := s.resolveMembership(ctx, userID, orgSlug)
	if err != nil {
		return false, err
	}
	val := "0"
	if allowed {
		val = "1"
	}
	_ = s.redis.Set(ctx, key, val, membershipCacheTTL).Err()
	return allowed, nil
}

func membershipCacheKey(userID, orgSlug string) string {
	return fmt.Sprintf("cli:member:%s:%s", userID, orgSlug)
}

// resolveMembership pages through the user's Clerk memberships looking for
// orgSlug. Stops early on match.
func (s *Service) resolveMembership(ctx context.Context, userID, orgSlug string) (bool, error) {
	limit := int64(100)
	var offset int64
	for {
		list, err := user.ListOrganizationMemberships(ctx, userID, &user.ListOrganizationMembershipsParams{
			ListParams: clerk.ListParams{Limit: &limit, Offset: &offset},
		})
		if err != nil {
			return false, fmt.Errorf("clerk list memberships: %w", err)
		}
		for _, m := range list.OrganizationMemberships {
			if m.Organization != nil && m.Organization.Slug == orgSlug {
				return true, nil
			}
		}
		got := int64(len(list.OrganizationMemberships))
		if got == 0 || offset+got >= list.TotalCount {
			return false, nil
		}
		offset += got
	}
}
