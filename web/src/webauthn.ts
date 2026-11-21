// WebAuthn ceremonies against /auth/... The server sends options with
// base64url fields; the browser API wants ArrayBuffers.

function b64url(s: string): ArrayBuffer {
  const b64 = s.replace(/-/g, '+').replace(/_/g, '/') + '==='.slice((s.length + 3) % 4)
  const bin = atob(b64)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out.buffer
}

function toB64url(buf: ArrayBuffer | null): string | undefined {
  if (!buf) return undefined
  const bytes = new Uint8Array(buf)
  let bin = ''
  for (const b of bytes) bin += String.fromCharCode(b)
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

async function postJSON(path: string, body: unknown): Promise<Response> {
  const r = await fetch(path, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!r.ok) {
    let msg = r.statusText
    try {
      msg = (await r.json()).error ?? msg
    } catch {
      /* not JSON */
    }
    throw new Error(msg)
  }
  return r
}

export async function register(token: string, name: string): Promise<void> {
  const opts = await (await postJSON('/auth/enroll/begin', { token, name })).json()
  const pk = opts.publicKey
  pk.challenge = b64url(pk.challenge)
  pk.user.id = b64url(pk.user.id)
  for (const c of pk.excludeCredentials ?? []) c.id = b64url(c.id)
  const cred = (await navigator.credentials.create({ publicKey: pk })) as PublicKeyCredential | null
  if (!cred) throw new Error('no passkey was created')
  const resp = cred.response as AuthenticatorAttestationResponse
  await postJSON('/auth/enroll/finish', {
    id: cred.id,
    rawId: toB64url(cred.rawId),
    type: cred.type,
    response: {
      clientDataJSON: toB64url(resp.clientDataJSON),
      attestationObject: toB64url(resp.attestationObject),
      transports: resp.getTransports?.() ?? [],
    },
    clientExtensionResults: cred.getClientExtensionResults(),
  })
}

export async function login(): Promise<void> {
  const opts = await (await postJSON('/auth/login/begin', {})).json()
  const pk = opts.publicKey
  pk.challenge = b64url(pk.challenge)
  for (const c of pk.allowCredentials ?? []) c.id = b64url(c.id)
  const cred = (await navigator.credentials.get({ publicKey: pk, mediation: opts.mediation || undefined })) as PublicKeyCredential | null
  if (!cred) throw new Error('no passkey was used')
  const resp = cred.response as AuthenticatorAssertionResponse
  await postJSON('/auth/login/finish', {
    id: cred.id,
    rawId: toB64url(cred.rawId),
    type: cred.type,
    response: {
      clientDataJSON: toB64url(resp.clientDataJSON),
      authenticatorData: toB64url(resp.authenticatorData),
      signature: toB64url(resp.signature),
      userHandle: toB64url(resp.userHandle),
    },
    clientExtensionResults: cred.getClientExtensionResults(),
  })
}
