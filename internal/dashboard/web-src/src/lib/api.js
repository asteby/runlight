// Tiny JSON client. Mutations carry the headers the server requires (Content-Type + X-Runlight).
export class ApiError extends Error {
  constructor(message, status = 0, fields = null) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.fields = fields; // { key: "mensaje" } on 400 validation errors
  }
}

async function request(method, path, body) {
  const init = { method, cache: 'no-store', credentials: 'same-origin', headers: { Accept: 'application/json' } };
  if (method !== 'GET') {
    init.headers['Content-Type'] = 'application/json';
    init.headers['X-Runlight'] = '1';
    init.body = JSON.stringify(body === undefined ? {} : body);
  }
  let r;
  try {
    r = await fetch(path, init);
  } catch {
    throw new ApiError('No se pudo conectar con el servidor.', 0);
  }
  if (r.status === 401) {
    location.replace('/login');
    throw new ApiError('Sesión requerida', 401);
  }
  if (r.status === 204) return null;
  let data = null;
  try {
    const text = await r.text();
    if (text) data = JSON.parse(text);
  } catch { /* non-JSON body */ }
  if (!r.ok) throw new ApiError((data && data.error) || 'Error ' + r.status, r.status, (data && data.fields) || null);
  return data;
}

export const api = {
  get: (p) => request('GET', p),
  post: (p, b) => request('POST', p, b),
  put: (p, b) => request('PUT', p, b),
  del: (p, b) => request('DELETE', p, b),
};
