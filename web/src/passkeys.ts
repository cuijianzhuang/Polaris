// Use WebAuthn directly; Safari versions without PublicKeyCredential.toJSON
// still need explicit ArrayBuffer/base64url conversion.
export const passkeysSupported = () => window.isSecureContext && typeof PublicKeyCredential !== 'undefined'
function decode(s: string): ArrayBuffer {
  const raw = atob(s.replace(/-/g, '+').replace(/_/g, '/'))
  return Uint8Array.from(raw, (c) => c.charCodeAt(0)).buffer
}
function encode(v: ArrayBuffer | null): string | null {
  if (!v) return null
  return btoa(String.fromCharCode(...new Uint8Array(v))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}
export async function createPasskey(options: { publicKey: any }) {
  const p = options.publicKey
  const credential = await navigator.credentials.create({ publicKey: { ...p, challenge: decode(p.challenge), user: { ...p.user, id: decode(p.user.id) }, excludeCredentials: p.excludeCredentials?.map((c: any) => ({ ...c, id: decode(c.id) })) } }) as PublicKeyCredential | null
  if (!credential) throw new Error('WebAuthn registration cancelled')
  const r = credential.response as AuthenticatorAttestationResponse
  return { id: credential.id, rawId: encode(credential.rawId), type: credential.type, authenticatorAttachment: credential.authenticatorAttachment, clientExtensionResults: credential.getClientExtensionResults(), response: { clientDataJSON: encode(r.clientDataJSON), attestationObject: encode(r.attestationObject), transports: r.getTransports?.() ?? [] } }
}
export async function usePasskey(options: { publicKey: any }) {
  const p = options.publicKey
  const credential = await navigator.credentials.get({ publicKey: { ...p, challenge: decode(p.challenge), allowCredentials: p.allowCredentials?.map((c: any) => ({ ...c, id: decode(c.id) })) } }) as PublicKeyCredential | null
  if (!credential) throw new Error('WebAuthn sign-in cancelled')
  const r = credential.response as AuthenticatorAssertionResponse
  return { id: credential.id, rawId: encode(credential.rawId), type: credential.type, authenticatorAttachment: credential.authenticatorAttachment, clientExtensionResults: credential.getClientExtensionResults(), response: { clientDataJSON: encode(r.clientDataJSON), authenticatorData: encode(r.authenticatorData), signature: encode(r.signature), userHandle: encode(r.userHandle) } }
}
