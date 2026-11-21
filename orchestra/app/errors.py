"""Only explicit client/domain failures are translated to HTTP 4xx responses."""


class RequestError(ValueError):
    pass


class NotFound(KeyError):
    pass
