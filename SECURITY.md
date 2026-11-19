# Security policy

## Supported versions

Security fixes go into the latest release of the library (`go.vicoworks.com/open-persona`) and of the platform. Before v1.0.0 only the latest minor version is supported: upgrade to receive fixes.

## Reporting a vulnerability

Do not open a public issue. Report it privately through GitHub's [private vulnerability reporting](https://github.com/VicoAritonang/open-persona/security/advisories/new), with the affected version, a description, and steps to reproduce if you have them.

You will get an acknowledgement within 3 working days and an assessment within 10. Fixes are released as a patch version and disclosed in a GitHub security advisory once a release is available; reporters are credited unless they prefer otherwise.

## Scope notes

- A `.persona` pack contains its training material verbatim. Anyone holding the file can read it; that is by design, not a vulnerability.
- `openpersona serve` without `OPENPERSONA_API_KEY` accepts unauthenticated requests and says so at startup. Bind it to localhost or put it behind authentication.
- Memory scopes are an isolation boundary only if your application derives them from its own authenticated session.
