# Zitadel Publisher authorization

This investigation records the earlier machine-Publisher migration. [ADR 0005](../adr/0005-project-qualified-roles-for-portal-access.md) supersedes its organization-specific policy and pre-migration live-check recommendation.

## Conclusion and scope

Source supports keeping existing service-account PAT clients and replacing ADR 0004's subject allowlist with a project-qualified role check. Introspection can supply both current role assignments and the account's human-readable name. No Action or separate administrative lookup is indicated by the inspected code. This is source evidence, not a verified result against the deployed version.

Settled scope: API Publishers only, one explicit role grants full rights, browser sign-in unchanged, authorization cache at most 60 seconds. Use the current account name for new publishes; leave historical `LastPublisher` strings unchanged. Return 503 when Zitadel is unavailable after cached authorization expires. This replaces ADR 0004's allowlist and configured labels, not its PAT transport.

Sources below pin implementation evidence to upstream commit `56f51f54534079259b2ee1594c4024c0ec421a1e`. The PAT audience/scopes helper and relevant userinfo behavior were also checked in release `v4.19.4`. Neither identifies the deployed version.

## Minimum mechanism

1. Keep the existing `hub-api` API application's introspection credentials. Confirm it belongs to the intended Zitadel project. Define a role such as `publisher` there and assign it to authorized service accounts in the intended organization.
2. Keep only global trust settings in Pub Hub: issuer/introspection endpoint, API credentials, exact project ID, exact authorization organization ID, role key. These are not per-user settings.
3. Require `active: true`, a nonempty `sub`, and this exact claim path:

   `urn:zitadel:iam:org:project:<projectID>:roles` → `publisher` → `<organizationID>`.

   The leaf value is an organization domain, not the authorization identifier. Do not authorize from `scope`, a bare role name, or `aud` alone. The official role guide recommends the project-qualified claim over the legacy unqualified claim.[1]
4. Use `sub` for identity and `name` for the new publish's display label. Cache the complete authorization/identity result, not merely token activity.

### Why PAT introspection can work

`Introspect` explicitly calls `assertClientScopesForPAT`, then `userInfo` with the introspecting client's project and `currentProjectOnly=true`.[2] The PAT helper appends every role definition in that project as `ScopeProjectRolePrefix+role.Key`.[3] These scopes request role information; they do not prove assignment.

`prepareRoles` selects the current project, and `assertRoles` builds claims from queried `UserGrants`.[4] The SQL filters `user_id`, instance, `project_id = any($3)` and `state = 1`.[5] Consequently an existing PAT should gain or lose the role claim as assignments change, without reissuance. This reads current projections, not an issuance-time grant snapshot. Projection delay plus Pub Hub's cache must be measured; source does not establish a strict 60-second end-to-end revocation guarantee.

For this PAT path, the source does not require enabling project "Assert Roles on Authentication", adding client-requested scopes, or changing OIDC token settings. The PAT helper supplies role scopes itself. Verify this on the deployed release before calling it the final configuration.

### Audience is not authorization

The decisive passage is `token.audience = append(token.audience, clientID, projectID)`.[3] Zitadel adds the introspecting application and project before validating audience.[2] An unrelated valid service-account PAT can therefore appear active with the target audience. This supports ADR 0004's warning despite the generic introspection guide's audience wording.[6]

Pin project and authorization organization IDs in the role check. The authorization organization differs conceptually from the user's home organization and the project's owning organization.[7] If the policy also requires a particular home organization, check `urn:zitadel:iam:user:resourceowner:id` separately; it is not a substitute for the role's organization key.

## Identity and labels

Official documentation says: "Human PATs are currently not supported."[8] The v2 handler explicitly restricts PATs to machines and includes `openid`, `profile`, user metadata and resource-owner scopes.[9] The management handler does the same.[10]

`Subject: user.User.ID` supplies stable identity. With `profile`, `userInfoProfileToOidc` maps machine `Name` to `name`, and `PreferredLoginName` to `preferred_username`; it also returns `updated_at`.[4] Renaming changes the queried label, not the user ID. Human OAuth tokens use `human.DisplayName`, given/family names and nickname, but there is no human-PAT case. No Action or lookup is necessary for the inspected normal PAT path; old PAT scope contents remain a test item.

## Fallback if deployed introspection differs

The least-privilege alternative is `AuthorizationService.ListAuthorizations` using the incoming PAT, filtered to its verified subject, project, organization and active state. Its contract says "no permissions required for listing own authorizations"; reading other users requires `user.grant.read`.[11] The response includes project, authorization organization, user IDs, role keys and display name.[7] This needs another request, pagination/error handling and coordinated caching, but no new server credential for self-lookup. The older `/auth/v1/usergrants/me/_search` is deprecated in favor of this API.[12] Do not grant `ORG_OWNER` merely to read assignments.

## Controlled test before implementation

Using an isolated test instance matching the deployed version, introspect one already-issued PAT through `hub-api`: no assignment; assign role; remove role; rename machine. Confirm exact claims, unchanged `sub`, and propagation time. Repeat with the same role key in another project and another authorization organization. Check existing PAT `profile` scope/name, expired/revoked PATs, disabled accounts and outage after cache expiry. Browser settings stay untouched: project role-check settings affect human login, whereas role assertion controls returned information.[13]

## Sources

[1]: https://zitadel.com/docs/guides/integrate/retrieve-user-roles
[2]: https://github.com/zitadel/zitadel/blob/56f51f54534079259b2ee1594c4024c0ec421a1e/internal/api/oidc/introspect.go
[3]: https://github.com/zitadel/zitadel/blob/56f51f54534079259b2ee1594c4024c0ec421a1e/internal/api/oidc/access_token.go
[4]: https://github.com/zitadel/zitadel/blob/56f51f54534079259b2ee1594c4024c0ec421a1e/internal/api/oidc/userinfo.go
[5]: https://github.com/zitadel/zitadel/blob/56f51f54534079259b2ee1594c4024c0ec421a1e/internal/query/userinfo_by_id.sql
[6]: https://zitadel.com/docs/guides/integrate/token-introspection/basic-auth
[7]: https://github.com/zitadel/zitadel/blob/56f51f54534079259b2ee1594c4024c0ec421a1e/proto/zitadel/authorization/v2/authorization.proto
[8]: https://zitadel.com/docs/guides/integrate/service-accounts/personal-access-token
[9]: https://github.com/zitadel/zitadel/blob/56f51f54534079259b2ee1594c4024c0ec421a1e/internal/api/grpc/user/v2/pat.go
[10]: https://github.com/zitadel/zitadel/blob/56f51f54534079259b2ee1594c4024c0ec421a1e/internal/api/grpc/management/user.go
[11]: https://github.com/zitadel/zitadel/blob/56f51f54534079259b2ee1594c4024c0ec421a1e/proto/zitadel/authorization/v2/authorization_service.proto
[12]: https://zitadel.com/docs/reference/api/auth/zitadel.auth.v1.AuthService.ListMyUserGrants
[13]: https://zitadel.com/docs/guides/manage/console/projects-overview#role-settings
