"""OpenRouter secrets live only in a native OS keyring, never in project config."""
SERVICE = 'agent-orchestra'
ACCOUNT = 'openrouter'


class CredentialError(RuntimeError):
    pass


def _vault():
    try:
        import keyring
    except ImportError:
        raise CredentialError('Install OpenRouter support: python -m pip install -e ".[openrouter]"') from None
    try:
        backend = keyring.get_keyring()
        if type(backend).__module__ not in {
            'keyring.backends.Windows', 'keyring.backends.macOS',
            'keyring.backends.SecretService', 'keyring.backends.kwallet',
        } or backend.priority <= 0:
            raise CredentialError('Select a native OS keyring backend; file-based credential storage is refused.')
    except CredentialError:
        raise
    except Exception:
        raise CredentialError('The OS keyring is unavailable.') from None
    return keyring


def get_key():
    vault = _vault()
    try:
        return vault.get_password(SERVICE, ACCOUNT)
    except Exception:
        raise CredentialError('Unable to read the OpenRouter key from the OS keyring.') from None


def set_key(value: str):
    if not isinstance(value, str) or not value.strip() or any(c.isspace() for c in value):
        raise CredentialError('The API key must be non-empty and contain no whitespace.')
    vault = _vault()
    try:
        vault.set_password(SERVICE, ACCOUNT, value)
    except Exception:
        raise CredentialError('Unable to save the OpenRouter key in the OS keyring.') from None
