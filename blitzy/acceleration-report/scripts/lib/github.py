"""Read-only GitHub REST API client for the acceleration-report pipeline.

This module is the canonical HTTP-side primitive consumed by the extraction
scripts ``03_extract_pulls.py``, ``04_extract_releases.py``,
``06_extract_ci_history.py``, and ``07_extract_exceptions.py``. It exposes
a single class :class:`GithubClient` that performs paginated GET requests
against the GitHub REST API at https://api.github.com (or any equivalent
base URL passed via constructor) with rate-limit-aware retry, exponential
back-off with jitter, RFC-5988 ``Link`` header pagination, and optional
on-disk cursor persistence for resume-on-failure.

Read-only enforcement
---------------------

The client is strictly read-only by construction. The only verb-issuing
private method, :meth:`GithubClient._request`, raises :class:`ValueError`
when the caller asks for any method other than ``GET``. The public
methods :meth:`GithubClient.get_one`, :meth:`GithubClient.get`,
:meth:`GithubClient.get_binary`, :meth:`GithubClient.paginate`, and
:meth:`GithubClient.paginate_endpoint` all dispatch through ``_request``
with a hard-coded ``"GET"`` argument, so no calling-side configuration can
escalate the verb. This enforcement makes Agent Action Plan §0.3.2 and
§0.8.2 read-only constraints structural rather than convention-based.

Retry and back-off
------------------

The retry loop attempts at most :data:`MAX_RETRIES` requests before
surfacing the last observed exception. Retries are triggered by:

* :class:`requests.exceptions.RequestException` subclasses (connection
  errors, timeouts, name-resolution failures);
* HTTP 429 Too Many Requests — sleep duration prefers the ``Retry-After``
  header, falls back to ``X-RateLimit-Reset`` epoch math, then defaults
  to the exponential back-off schedule used for other transient errors;
* HTTP 5xx server errors — exponential back-off with ±20% jitter.

HTTP 4xx other than 429 is treated as a permanent client-side problem
(invalid path, missing scope, unknown repository) and is surfaced
immediately via :meth:`requests.Response.raise_for_status` after a
structured log event is emitted.

Dependency surface
------------------

The module imports only the Python standard library and the
``requests`` package pinned at the version declared in
``blitzy/acceleration-report/requirements.txt``. It does NOT import any
other ``lib/`` module — the logger is supplied via constructor parameter
(dependency injection) so that this module remains a peripheral library
that introduces no cycles into the extraction-pipeline import graph.

Secret hygiene
--------------

The constructor accepts a token through the ``token`` parameter. The
token is held privately as ``self._token`` and used only to construct
the ``Authorization`` header on the underlying :class:`requests.Session`.
The token value is never passed to the logger; the
``github_client_initialized`` event records only a boolean
``authenticated`` flag. Callers that need to log token-related metadata
should rely on the logger-layer redaction documented in
``lib/observability.py``.
"""

from __future__ import annotations

import json
import os
import random
import re
import time
from pathlib import Path
from typing import Any, Iterator
from urllib.parse import urlparse

import requests


# ---------------------------------------------------------------------------
# Module-level constants
# ---------------------------------------------------------------------------


#: Base URL of the public GitHub REST API. Surfaced as a module-level constant
#: so that callers can substitute a GitHub Enterprise Server endpoint by
#: passing ``base_url`` to the :class:`GithubClient` constructor without
#: editing this file.
DEFAULT_BASE_URL: str = "https://api.github.com"

#: Value sent in the ``X-GitHub-Api-Version`` request header on every
#: invocation. Pinning the API version prevents drift between extraction
#: runs and is the contract recommended by GitHub for production clients.
DEFAULT_API_VERSION: str = "2022-11-28"

#: Value sent in the ``User-Agent`` request header on every invocation.
#: GitHub returns 403 Forbidden for unauthenticated requests with no
#: User-Agent; supplying a descriptive identifier here both satisfies that
#: requirement and tags the analysis pipeline's traffic in any server-side
#: access log a repository administrator might consult.
DEFAULT_USER_AGENT: str = "blitzy-acceleration-report/1.0"

#: Default value for the ``per_page`` query parameter. GitHub caps the
#: page size at 100 for most endpoints; matching that ceiling minimises
#: the number of round-trips and the number of cursor checkpoints
#: written during pagination.
DEFAULT_PAGE_SIZE: int = 100

#: Maximum number of attempts the retry loop performs before surfacing
#: the last observed exception. Five attempts with exponential back-off
#: capped at :data:`BACKOFF_MAX_SECONDS` yields a worst-case wait of
#: approximately ``1 + 2 + 4 + 8 + 16 = 31`` seconds before the loop
#: gives up, which is a reasonable upper bound for transient GitHub
#: API hiccups.
MAX_RETRIES: int = 5

#: Initial sleep duration (in seconds) used by the exponential back-off
#: schedule. Attempt ``n`` sleeps ``BACKOFF_BASE_SECONDS * (2 ** n)``
#: seconds, capped at :data:`BACKOFF_MAX_SECONDS`, multiplied by a
#: ``random.uniform(0.8, 1.2)`` jitter factor.
BACKOFF_BASE_SECONDS: float = 1.0

#: Cap on the exponential back-off sleep duration. Even after the
#: doubling sequence exceeds this value the actual sleep stays bounded,
#: preventing pathological delays when an outage persists.
BACKOFF_MAX_SECONDS: float = 16.0

#: Threshold on ``X-RateLimit-Remaining`` below which the client
#: preemptively sleeps until the rate-limit window resets. Setting this
#: above zero keeps headroom for any other process sharing the same
#: GitHub token from being starved by this pipeline.
RATE_LIMIT_LOW_THRESHOLD: int = 10

#: Number of seconds added to the rate-limit reset epoch when computing
#: the sleep duration. The buffer absorbs clock skew between the local
#: machine and GitHub's rate-limit accounting and guarantees the next
#: request lands inside the reset window rather than racing the boundary.
RATE_LIMIT_BUFFER_SECONDS: int = 5

#: Per-request timeout in seconds passed to :meth:`requests.Session.request`.
#: GitHub responses for paginated list endpoints typically arrive in well
#: under one second; thirty seconds is a wide safety margin that still
#: prevents indefinite hangs.
REQUEST_TIMEOUT_SECONDS: int = 30

#: Regex matching the ``rel="next"`` segment of an RFC-5988 ``Link``
#: header. The capture group extracts the URL between the leading ``<``
#: and trailing ``>`` characters.
_LINK_NEXT_PATTERN: re.Pattern[str] = re.compile(
    r'<([^>]+)>\s*;\s*rel\s*=\s*"?next"?', re.IGNORECASE
)

#: Regex matching an embedded ``user:password@`` or ``token@`` segment in a
#: URL. Used by :func:`redact_url` to remove credentials before any URL is
#: passed to the logger or otherwise written to an artifact.
_BASIC_AUTH_PATTERN: re.Pattern[str] = re.compile(r"https?://[^@/\s]+@")

#: Maximum length of the response body excerpt embedded in the
#: ``github_api_4xx`` log event. Long bodies are truncated so the JSON
#: log line stays readable and the file does not balloon when a 404
#: returns a verbose error page.
_BODY_EXCERPT_MAX_CHARS: int = 512


# ---------------------------------------------------------------------------
# GithubClient class
# ---------------------------------------------------------------------------


class GithubClient:
    """HTTP client that performs read-only GitHub REST API requests.

    Instances of this class wrap a :class:`requests.Session` configured with
    the default Accept, API-version, and User-Agent headers documented at
    the module level. When a token is supplied, an ``Authorization: Bearer``
    header is also installed; the token value itself is held privately on
    the instance and is never passed to the logger.

    The client exposes three calling shapes:

    * :meth:`get_one` — fetch a single JSON object (e.g. ``/rate_limit``,
      ``/repos/{owner}/{repo}``).
    * :meth:`get` — fetch a raw :class:`requests.Response` so the caller
      can inspect headers (including the ``Link`` header for pagination).
    * :meth:`paginate` / :meth:`paginate_endpoint` — yield items from a
      paginated endpoint until the ``next`` link is exhausted.

    A binary-content helper :meth:`get_binary` is provided for endpoints
    such as the GitHub Actions artifact download URL, which returns a
    compressed archive rather than JSON.

    The class is not thread-safe. Each thread should construct its own
    instance. The underlying :class:`requests.Session` pools connections
    per-instance.

    Example:
        >>> import logging
        >>> client = GithubClient(token=None, logger=logging.getLogger("smoke"))
        >>> payload = client.get_one("rate_limit")
        >>> payload["resources"]["core"]["limit"]  # doctest: +SKIP
        60
    """

    def __init__(
        self,
        token: str | None,
        logger: Any,
        base_url: str = DEFAULT_BASE_URL,
        cursor_path: Path | None = None,
    ) -> None:
        """Initialise the client.

        Args:
            token: GitHub Personal Access Token, OAuth token, or GitHub App
                installation token. When ``None``, the client makes
                unauthenticated requests subject to the public rate limit
                (60 requests per hour at the time of writing). The value
                is stored privately and is never logged.
            logger: Any object exposing the :class:`logging.Logger`
                interface — at minimum ``debug``, ``info``, ``warning``,
                and ``error`` methods accepting a message string and an
                ``extra`` keyword argument. The recommended source is
                ``lib.observability.get_logger(script_name)``, but the
                dependency-injection signature lets tests pass a stub
                without importing the observability module.
            base_url: GitHub REST API base URL. Trailing ``/`` is stripped.
                Defaults to :data:`DEFAULT_BASE_URL`. Override for GitHub
                Enterprise Server.
            cursor_path: Optional filesystem path where the next-page URL
                will be persisted after each successful page during
                pagination. When supplied, the path is written atomically
                via temp-file rename. A subsequent run can read this file
                to resume pagination from the recorded checkpoint. ``None``
                disables persistence (the default behaviour for one-shot
                runs that do not need resume-on-failure).
        """
        self._token: str | None = token
        self._logger: Any = logger
        self._base_url: str = base_url.rstrip("/")
        self._cursor_path: Path | None = cursor_path

        self._session: requests.Session = requests.Session()
        self._session.headers.update(
            {
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": DEFAULT_API_VERSION,
                "User-Agent": DEFAULT_USER_AGENT,
            }
        )
        if token is not None:
            # Authorization header carries the token. The token value
            # itself is intentionally not echoed through the logger.
            self._session.headers["Authorization"] = f"Bearer {token}"

        self._logger.info(
            "github_client_initialized",
            extra={
                "base_url": self._base_url,
                "authenticated": token is not None,
                "cursor_persisted": cursor_path is not None,
            },
        )

    # ------------------------------------------------------------------
    # URL composition
    # ------------------------------------------------------------------

    def _build_url(self, path: str) -> str:
        """Resolve ``path`` to a fully qualified GitHub API URL.

        The helper accepts both bare API paths (e.g. ``"rate_limit"``,
        ``"/repos/octocat/Hello-World/pulls"``) and pre-resolved absolute
        URLs (e.g. the value extracted from a ``Link: rel="next"`` header).
        The latter shape is required by :meth:`paginate`, which forwards
        the next-page URL verbatim to :meth:`_request` without rebuilding.

        Args:
            path: A path beginning with ``/`` or without one, or a full
                ``http://`` / ``https://`` URL.

        Returns:
            The absolute URL ready for use with
            :meth:`requests.Session.request`.
        """
        if path.startswith("https://") or path.startswith("http://"):
            return path
        return f"{self._base_url}/{path.lstrip('/')}"

    # ------------------------------------------------------------------
    # Back-off helpers
    # ------------------------------------------------------------------

    @staticmethod
    def _exponential_backoff_seconds(attempt: int) -> float:
        """Return the sleep duration for the given attempt with jitter.

        Computes ``BACKOFF_BASE_SECONDS * 2 ** attempt``, caps the result
        at :data:`BACKOFF_MAX_SECONDS`, then multiplies by a uniformly
        distributed jitter factor in the range ``[0.8, 1.2]``. The jitter
        prevents thundering-herd retries when multiple concurrent runs of
        the pipeline hit the same transient outage at the same moment.

        Args:
            attempt: Zero-indexed attempt counter. ``0`` is the first
                retry following the initial failed request.

        Returns:
            Sleep duration in seconds, always > 0 and <=
            ``BACKOFF_MAX_SECONDS * 1.2``.
        """
        base = BACKOFF_BASE_SECONDS * (2 ** attempt)
        capped = min(base, BACKOFF_MAX_SECONDS)
        jitter = random.uniform(0.8, 1.2)
        return capped * jitter

    @staticmethod
    def _retry_after_seconds(response: requests.Response) -> float | None:
        """Return the number of seconds to wait from the ``Retry-After`` header.

        GitHub uses the ``Retry-After`` header to communicate a
        server-suggested wait duration on 429 and certain 503 responses.
        The header is documented as either an integer number of seconds
        or an HTTP-date; only the integer form is observed in practice
        from GitHub. This helper parses the integer form and returns it
        as a float; it returns ``None`` when the header is absent or
        malformed so the caller can fall through to the fallback math.

        Args:
            response: The HTTP response carrying the optional header.

        Returns:
            Seconds to sleep, or ``None`` if the header is missing or
            unparseable.
        """
        value = response.headers.get("Retry-After")
        if value is None:
            return None
        try:
            return float(value)
        except (TypeError, ValueError):
            return None

    @staticmethod
    def _rate_limit_reset_seconds(response: requests.Response) -> float | None:
        """Return seconds-until-reset computed from the rate-limit header.

        GitHub responses always carry an ``X-RateLimit-Reset`` header
        whose value is the epoch second at which the current rate-limit
        bucket refills. Sleep duration is computed as ``reset_epoch -
        time.time() + RATE_LIMIT_BUFFER_SECONDS``, clamped to a minimum
        of zero. When the header is absent or the value is in the past
        or unparseable, the function returns ``None`` so the caller can
        fall through to exponential back-off.

        Args:
            response: The HTTP response carrying the rate-limit header.

        Returns:
            Non-negative seconds-until-reset, or ``None`` when the header
            is missing or unparseable.
        """
        value = response.headers.get("X-RateLimit-Reset")
        if value is None:
            return None
        try:
            reset_epoch = float(value)
        except (TypeError, ValueError):
            return None
        delta = reset_epoch - time.time() + RATE_LIMIT_BUFFER_SECONDS
        if delta < 0:
            return 0.0
        return delta

    def _maybe_sleep_for_low_rate_limit(
        self, response: requests.Response
    ) -> None:
        """Preemptively sleep when the rate-limit bucket is nearly drained.

        Inspects ``X-RateLimit-Remaining``; when the value is set and
        integer-convertible and strictly less than
        :data:`RATE_LIMIT_LOW_THRESHOLD`, the method computes the
        seconds-until-reset and sleeps that long. This trades a small
        amount of wall-clock time on the current run for guaranteed
        availability of the next page without the caller having to
        special-case the 429 path. The decision and duration are logged
        under the sentinel event ``github_api_rate_limit_low``.

        Args:
            response: The most recent successful response.
        """
        remaining_str = response.headers.get("X-RateLimit-Remaining")
        if remaining_str is None:
            return
        try:
            remaining = int(remaining_str)
        except (TypeError, ValueError):
            return
        if remaining >= RATE_LIMIT_LOW_THRESHOLD:
            return
        sleep_seconds = self._rate_limit_reset_seconds(response) or 0.0
        self._logger.warning(
            "github_api_rate_limit_low",
            extra={
                "rate_limit_remaining": remaining,
                "sleep_seconds": sleep_seconds,
            },
        )
        if sleep_seconds > 0:
            time.sleep(sleep_seconds)

    # ------------------------------------------------------------------
    # Core request loop
    # ------------------------------------------------------------------

    def _request(
        self,
        method: str,
        url: str,
        params: dict[str, Any] | None = None,
    ) -> requests.Response:
        """Issue a single GitHub API request, with retry and rate-limit handling.

        The method is the only verb-issuing surface in the class. It
        accepts a ``method`` parameter solely so the read-only invariant
        can be enforced explicitly: passing anything other than ``"GET"``
        raises :class:`ValueError`. Callers should always pass
        ``"GET"`` — the public :meth:`get_one`, :meth:`get`, and
        :meth:`get_binary` methods do this for them.

        Retry behaviour:

        * Network errors (any :class:`requests.exceptions.RequestException`)
          trigger a retry with exponential back-off and jitter.
        * HTTP 429 triggers a retry; sleep duration prefers
          ``Retry-After``, falls back to ``X-RateLimit-Reset`` math,
          then to exponential back-off.
        * HTTP 5xx triggers a retry with exponential back-off and jitter.
        * HTTP 4xx other than 429 is raised immediately via
          :meth:`requests.Response.raise_for_status`.
        * HTTP 2xx returns the response, after preemptively sleeping if
          the remaining rate-limit bucket falls below the configured
          low-water threshold.

        Args:
            method: HTTP method. Must be ``"GET"``. Any other value
                raises :class:`ValueError`.
            url: Absolute URL to request. The :meth:`_build_url` helper
                should be applied to API paths before they reach here.
            params: Optional query-string parameters. Forwarded verbatim
                to :meth:`requests.Session.request`. ``None`` means no
                query string is appended.

        Returns:
            The successful :class:`requests.Response`. The response
            body is not consumed by this method; callers are responsible
            for ``.json()`` / ``.content`` extraction.

        Raises:
            ValueError: If ``method`` is anything other than ``"GET"``.
            requests.HTTPError: On a 4xx response other than 429, or on
                any retry-exhausted 429/5xx response.
            requests.exceptions.RequestException: On a retry-exhausted
                network-level failure (connection refused, DNS error,
                timeout, etc.).
        """
        if method != "GET":
            raise ValueError(
                "GithubClient is read-only; only GET requests are permitted"
            )

        last_exception: BaseException | None = None
        for attempt in range(MAX_RETRIES):
            try:
                resp = self._session.request(
                    "GET",
                    url,
                    params=params,
                    timeout=REQUEST_TIMEOUT_SECONDS,
                )
            except requests.exceptions.RequestException as exc:
                last_exception = exc
                sleep_seconds = self._exponential_backoff_seconds(attempt)
                self._logger.warning(
                    "github_api_network_error",
                    extra={
                        "attempt": attempt + 1,
                        "max_retries": MAX_RETRIES,
                        "url": redact_url(url),
                        "error": str(exc),
                        "error_type": type(exc).__name__,
                        "sleep_seconds": sleep_seconds,
                    },
                )
                time.sleep(sleep_seconds)
                continue

            status = resp.status_code

            if 200 <= status < 300:
                self._logger.debug(
                    "github_api_request",
                    extra={
                        "url": redact_url(url),
                        "status": status,
                        "attempt": attempt + 1,
                        "rate_limit_remaining": resp.headers.get(
                            "X-RateLimit-Remaining"
                        ),
                    },
                )
                self._maybe_sleep_for_low_rate_limit(resp)
                return resp

            if status == 429:
                last_exception = requests.HTTPError(
                    f"429 Too Many Requests from {redact_url(url)}",
                    response=resp,
                )
                retry_after = self._retry_after_seconds(resp)
                if retry_after is not None:
                    sleep_seconds = retry_after
                else:
                    reset_seconds = self._rate_limit_reset_seconds(resp)
                    sleep_seconds = (
                        reset_seconds
                        if reset_seconds is not None
                        else self._exponential_backoff_seconds(attempt)
                    )
                self._logger.warning(
                    "github_api_rate_limited",
                    extra={
                        "attempt": attempt + 1,
                        "max_retries": MAX_RETRIES,
                        "url": redact_url(url),
                        "sleep_seconds": sleep_seconds,
                        "rate_limit_remaining": resp.headers.get(
                            "X-RateLimit-Remaining"
                        ),
                        "rate_limit_reset": resp.headers.get(
                            "X-RateLimit-Reset"
                        ),
                    },
                )
                time.sleep(sleep_seconds)
                continue

            if 500 <= status < 600:
                last_exception = requests.HTTPError(
                    f"{status} server error from {redact_url(url)}",
                    response=resp,
                )
                sleep_seconds = self._exponential_backoff_seconds(attempt)
                self._logger.warning(
                    "github_api_5xx_retry",
                    extra={
                        "attempt": attempt + 1,
                        "max_retries": MAX_RETRIES,
                        "url": redact_url(url),
                        "status": status,
                        "sleep_seconds": sleep_seconds,
                    },
                )
                time.sleep(sleep_seconds)
                continue

            # Any other 4xx is treated as a permanent client-side problem:
            # invalid path, missing scope, unknown repository, etc. Retry
            # cannot fix these; surface immediately after logging a body
            # excerpt to aid debugging.
            body_excerpt = ""
            try:
                body_excerpt = resp.text[:_BODY_EXCERPT_MAX_CHARS]
            except (UnicodeDecodeError, AttributeError):
                body_excerpt = "<binary or undecodable body>"
            self._logger.error(
                "github_api_4xx",
                extra={
                    "url": redact_url(url),
                    "status": status,
                    "body_excerpt": body_excerpt,
                },
            )
            resp.raise_for_status()
            # raise_for_status() will have raised for any non-2xx; the
            # following return is unreachable but kept for type-checker
            # completeness.
            return resp

        # Retry budget exhausted. Surface the most recent exception so the
        # caller observes a typed failure rather than a None return.
        self._logger.error(
            "github_api_failed",
            extra={
                "url": redact_url(url),
                "attempts": MAX_RETRIES,
                "last_error": str(last_exception)
                if last_exception is not None
                else None,
                "last_error_type": type(last_exception).__name__
                if last_exception is not None
                else None,
            },
        )
        if last_exception is not None:
            raise last_exception
        # Defensive: the loop body always assigns ``last_exception`` before
        # ``continue``, so reaching this branch would indicate a logic bug
        # rather than a normal failure mode.
        raise requests.HTTPError(
            f"GithubClient exhausted {MAX_RETRIES} retries without a "
            f"successful response from {redact_url(url)}"
        )

    # ------------------------------------------------------------------
    # Public read accessors
    # ------------------------------------------------------------------

    def get_one(
        self,
        path: str,
        params: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Fetch a single JSON object from the GitHub REST API.

        Convenience wrapper around :meth:`_request` for endpoints that
        return a single object (e.g. ``/rate_limit``,
        ``/repos/{owner}/{repo}``, ``/repos/{owner}/{repo}/branches/{branch}``).
        The response body is parsed as JSON and the resulting dictionary
        is returned to the caller.

        Args:
            path: API path or absolute URL; passed through
                :meth:`_build_url` before dispatch.
            params: Optional query-string parameters.

        Returns:
            The parsed JSON object. Always a dictionary because this
            method is intended for object-returning endpoints; if the
            endpoint returns a list, callers should use :meth:`get` or
            :meth:`paginate_endpoint` instead.

        Raises:
            ValueError: If the response body is not parseable as JSON
                or is not a JSON object (e.g. the endpoint returns a
                bare list).
            requests.HTTPError: On a 4xx response other than 429, or on
                any retry-exhausted 429/5xx response.
            requests.exceptions.RequestException: On a retry-exhausted
                network-level failure.
        """
        resp = self._request("GET", self._build_url(path), params=params)
        try:
            payload = resp.json()
        except json.JSONDecodeError as exc:
            raise ValueError(
                f"GitHub API response from {redact_url(resp.url)} is not "
                f"valid JSON: {exc}"
            ) from exc
        if not isinstance(payload, dict):
            raise ValueError(
                f"GitHub API response from {redact_url(resp.url)} is not "
                f"a JSON object; got {type(payload).__name__}"
            )
        return payload

    def get(
        self,
        path: str,
        params: dict[str, Any] | None = None,
    ) -> requests.Response:
        """Fetch a raw :class:`requests.Response` from the GitHub REST API.

        Used by callers that need access to response headers in addition
        to the body, including the ``Link`` header consumed by the
        pagination methods and the audit-log endpoint which returns its
        next-page cursor in a header rather than the body.

        Args:
            path: API path or absolute URL; passed through
                :meth:`_build_url` before dispatch.
            params: Optional query-string parameters.

        Returns:
            The :class:`requests.Response` returned by the underlying
            request. The body is not yet read; callers must call
            ``.json()`` or ``.content`` explicitly.

        Raises:
            requests.HTTPError: On a 4xx response other than 429, or on
                any retry-exhausted 429/5xx response.
            requests.exceptions.RequestException: On a retry-exhausted
                network-level failure.
        """
        return self._request("GET", self._build_url(path), params=params)

    def get_binary(self, url: str) -> bytes:
        """Fetch raw binary content from an absolute URL.

        Used for endpoints whose response body is not JSON, such as the
        artifact-archive download URL returned by
        ``GET /repos/{owner}/{repo}/actions/artifacts/{id}/zip``.
        The ``url`` parameter is treated as already-absolute — the
        artifact endpoint returns a 302 redirect to an S3 URL with a
        signed query string, and that signed URL must not be rebuilt by
        :meth:`_build_url`.

        Args:
            url: Absolute URL pointing at the binary resource.

        Returns:
            The raw response body as bytes.

        Raises:
            requests.HTTPError: On a 4xx response other than 429, or on
                any retry-exhausted 429/5xx response.
            requests.exceptions.RequestException: On a retry-exhausted
                network-level failure.
        """
        resp = self._request("GET", url)
        return resp.content

    # ------------------------------------------------------------------
    # Pagination
    # ------------------------------------------------------------------

    @staticmethod
    def _next_url_from_link_header(link_header: str | None) -> str | None:
        """Extract the ``rel="next"`` URL from an RFC-5988 Link header.

        GitHub's paginated endpoints return a ``Link`` header containing
        zero or more URL-rel pairs in the format
        ``<url>; rel="next", <url>; rel="last"``. This helper parses
        the header and returns the URL whose rel is ``next``, or
        ``None`` when the header is missing, empty, or contains no
        ``next`` entry.

        The parser is intentionally permissive on whitespace and
        quoting around the rel value: it accepts both quoted
        (``rel="next"``) and unquoted (``rel=next``) forms.

        Args:
            link_header: The raw ``Link`` header string, or ``None``.

        Returns:
            The next-page URL when present, otherwise ``None``.
        """
        if not link_header:
            return None
        match = _LINK_NEXT_PATTERN.search(link_header)
        if match is None:
            return None
        return match.group(1).strip()

    def _persist_cursor(self, next_url: str | None) -> None:
        """Atomically write the next-page URL to the cursor file.

        Called after every page is yielded to disk so that an
        interrupted run can be resumed without restarting from the
        first page. The write is atomic with respect to crashes: a
        sibling temp file receives the new value first, then
        :func:`os.replace` moves it over the target path. On platforms
        where ``os.replace`` is atomic (Linux ext4, macOS APFS,
        Windows NTFS) this guarantees that the cursor file is never
        observed in a half-written state.

        When ``self._cursor_path`` is ``None`` the method is a no-op.

        Args:
            next_url: The URL of the next page to fetch on resume, or
                ``None`` to record that pagination has completed.
        """
        if self._cursor_path is None:
            return
        payload = json.dumps(
            {"next_url": next_url, "ts": time.time()},
            ensure_ascii=False,
        )
        tmp_path = self._cursor_path.with_suffix(
            self._cursor_path.suffix + ".tmp"
        )
        try:
            tmp_path.parent.mkdir(parents=True, exist_ok=True)
            tmp_path.write_text(payload, encoding="utf-8")
            os.replace(tmp_path, self._cursor_path)
        except OSError as exc:
            # Cursor persistence is a courtesy for resume; failure to
            # write it must not derail an otherwise successful page
            # fetch. Log the failure and continue.
            self._logger.warning(
                "github_cursor_write_failed",
                extra={
                    "cursor_path": str(self._cursor_path),
                    "error": str(exc),
                    "error_type": type(exc).__name__,
                },
            )

    def paginate(
        self,
        response: requests.Response,
        item_key: str | None = None,
    ) -> Iterator[dict[str, Any]]:
        """Yield items from a paginated GitHub endpoint.

        Given an initial response from one of the list endpoints, the
        generator yields items from the current page, follows the
        ``rel="next"`` link to retrieve the next page, and repeats
        until no ``next`` link is present. Each subsequent page is
        fetched via :meth:`_request`, so retry and rate-limit handling
        applies to every page of a multi-page traversal — not only the
        first.

        Two response shapes are supported via the ``item_key``
        parameter:

        * Bare-array endpoints (``/pulls``, ``/issues``,
          ``/releases``): ``item_key`` is ``None``; the body is a JSON
          array and each element is yielded directly.
        * Wrapped-object endpoints (``/actions/runs``,
          ``/actions/runs/{id}/artifacts``): ``item_key`` selects the
          inner list field (``"workflow_runs"``, ``"artifacts"``).

        Args:
            response: The initial response. Typically obtained via
                :meth:`get` or :meth:`paginate_endpoint`.
            item_key: When set, the name of the JSON field on each
                page containing the list of items. When ``None``, the
                page body must itself be a list.

        Yields:
            Items from each successive page in the order returned by
            GitHub.

        Raises:
            ValueError: If a page body cannot be decoded as JSON, or
                if the shape does not match the ``item_key`` argument
                (e.g. ``item_key`` is ``None`` but the body is not a
                list, or ``item_key`` is set but the body lacks that
                field).
            requests.HTTPError: On a 4xx response other than 429, or on
                any retry-exhausted 429/5xx response.
            requests.exceptions.RequestException: On a retry-exhausted
                network-level failure.
        """
        current: requests.Response = response
        while True:
            try:
                payload = current.json()
            except json.JSONDecodeError as exc:
                raise ValueError(
                    f"GitHub API page from {redact_url(current.url)} is "
                    f"not valid JSON: {exc}"
                ) from exc

            items: list[Any]
            if item_key is None:
                if not isinstance(payload, list):
                    raise ValueError(
                        f"GitHub API page from {redact_url(current.url)} "
                        f"expected a JSON array but received "
                        f"{type(payload).__name__}"
                    )
                items = payload
            else:
                if not isinstance(payload, dict):
                    raise ValueError(
                        f"GitHub API page from {redact_url(current.url)} "
                        f"expected a JSON object with key {item_key!r} "
                        f"but received {type(payload).__name__}"
                    )
                inner = payload.get(item_key)
                if not isinstance(inner, list):
                    raise ValueError(
                        f"GitHub API page from {redact_url(current.url)} "
                        f"has no list under key {item_key!r}; got "
                        f"{type(inner).__name__}"
                    )
                items = inner

            for item in items:
                yield item

            next_url = self._next_url_from_link_header(
                current.headers.get("Link")
            )
            self._persist_cursor(next_url)
            if next_url is None:
                return
            current = self._request("GET", next_url)

    def paginate_endpoint(
        self,
        path: str,
        params: dict[str, Any] | None = None,
        item_key: str | None = None,
    ) -> Iterator[dict[str, Any]]:
        """Yield items from a paginated endpoint identified by API path.

        Combines :meth:`get` and :meth:`paginate` into a single helper
        for the common case where the caller does not need to inspect
        the initial response separately. The query string is
        populated with a default ``per_page`` of :data:`DEFAULT_PAGE_SIZE`
        when the caller has not supplied one explicitly, minimising
        round-trips on large collections.

        Args:
            path: API path or absolute URL. Typically a relative path
                such as ``"/repos/{owner}/{repo}/pulls"``.
            params: Optional query-string parameters. A shallow copy is
                made before defaulting ``per_page`` so the caller's
                dictionary is not mutated.
            item_key: Optional inner-list field name for wrapped-object
                response shapes. See :meth:`paginate` for details.

        Yields:
            Items from each successive page.

        Raises:
            ValueError: Same conditions as :meth:`paginate`.
            requests.HTTPError: Same conditions as :meth:`paginate`.
            requests.exceptions.RequestException: Same conditions as
                :meth:`paginate`.
        """
        effective_params: dict[str, Any] = dict(params) if params else {}
        if "per_page" not in effective_params:
            effective_params["per_page"] = DEFAULT_PAGE_SIZE
        initial = self.get(path, params=effective_params)
        yield from self.paginate(initial, item_key=item_key)


# ---------------------------------------------------------------------------
# Helper functions
# ---------------------------------------------------------------------------


def parse_repo_slug(slug: str) -> tuple[str, str]:
    """Split a ``owner/repo`` slug into its component segments.

    Accepts strings of the form ``"Owner/Repo"`` and returns the
    ``(owner, repo)`` tuple after stripping leading and trailing
    whitespace from each segment. Empty segments and slugs without a
    ``/`` separator are rejected with :class:`ValueError` so that
    misconfigured ``GITHUB_REPO_SLUG`` values surface as a typed error
    rather than silently producing nonsensical API URLs.

    Args:
        slug: A ``owner/repo`` slug, e.g.
            ``"Blitzy-Sandbox/blitzy-RudderStack"``.

    Returns:
        Two-element tuple ``(owner, repo)``.

    Raises:
        ValueError: If ``slug`` is empty, lacks a ``/`` separator,
            contains more than one ``/`` separator, or has an empty
            segment on either side of the separator.

    Example:
        >>> parse_repo_slug("Blitzy-Sandbox/blitzy-RudderStack")
        ('Blitzy-Sandbox', 'blitzy-RudderStack')
    """
    if not isinstance(slug, str) or not slug:
        raise ValueError(
            "repo slug must be a non-empty string of the form 'owner/repo'"
        )
    if "/" not in slug:
        raise ValueError(
            f"repo slug {slug!r} is missing the '/' separator; "
            "expected 'owner/repo'"
        )
    parts = slug.split("/")
    if len(parts) != 2:
        raise ValueError(
            f"repo slug {slug!r} has {len(parts)} segments; "
            "expected exactly two ('owner/repo')"
        )
    owner, repo = parts[0].strip(), parts[1].strip()
    if not owner or not repo:
        raise ValueError(
            f"repo slug {slug!r} has an empty segment; "
            "expected 'owner/repo' with non-empty values"
        )
    return owner, repo


def redact_url(url: str) -> str:
    """Remove any embedded basic-auth or token segment from a URL.

    URLs constructed by upstream tooling occasionally carry credentials
    in the ``https://user:password@host/...`` or ``https://token@host/...``
    form. Such URLs are unsafe to write to a log line or persisted
    artifact because the credential is then exposed downstream. This
    helper strips the credential segment while preserving the scheme,
    host, path, and query string, and is the canonical sanitiser called
    by every URL-bearing log event emitted from :class:`GithubClient`.

    The implementation pattern is documented in the AAP §0.4.2 entry
    for the ``re`` standard-library import: a single regex substitution
    of ``r"https?://[^@/]+@"`` with ``f"{scheme}://"`` extracted from
    :func:`urllib.parse.urlparse`. When the URL has no embedded
    credential the input is returned unchanged.

    Args:
        url: A URL string. ``None`` and non-string inputs raise
            :class:`TypeError` rather than silently passing through, so
            that mis-typed callers surface their bug immediately.

    Returns:
        The URL with any embedded credential segment removed.

    Raises:
        TypeError: If ``url`` is not a string.

    Example:
        >>> redact_url("https://ghp_secret@api.github.com/repos/octocat")
        'https://api.github.com/repos/octocat'
        >>> redact_url("https://api.github.com/rate_limit")
        'https://api.github.com/rate_limit'
    """
    if not isinstance(url, str):
        raise TypeError(
            f"redact_url expected a string, got {type(url).__name__}"
        )
    if _BASIC_AUTH_PATTERN.search(url) is None:
        return url
    # Derive the scheme from the URL parser so that the replacement
    # honours whatever scheme the caller used (http vs https) without
    # hardcoding either. The parsed result is otherwise unused; the
    # regex substitution does the real work.
    parsed = urlparse(url)
    scheme = parsed.scheme if parsed.scheme else "https"
    return _BASIC_AUTH_PATTERN.sub(f"{scheme}://", url, count=1)


# ---------------------------------------------------------------------------
# Public API surface
# ---------------------------------------------------------------------------

#: Tuple of public names exported by this module. The contract surface for
#: consumers is exhaustively enumerated here; nothing else in the module is
#: part of the documented public API. Underscore-prefixed names remain
#: importable for tests but are not part of the documented surface.
__all__ = [
    "GithubClient",
    "parse_repo_slug",
    "redact_url",
    "DEFAULT_BASE_URL",
    "DEFAULT_API_VERSION",
    "DEFAULT_PAGE_SIZE",
    "MAX_RETRIES",
]
