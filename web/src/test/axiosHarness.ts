import { AxiosError, type AxiosResponse, type InternalAxiosRequestConfig } from 'axios';

/**
 * The shared axios harness for the console's unit tests.
 *
 * A real adapter at the bottom of the pipeline, not a mock of the client: the
 * interceptors in `api/client.ts` (bearer, single-flight renewal, the auth
 * exemptions) only run on a request axios actually processes, and a page test
 * that mocked `client.get` would prove nothing about them.
 *
 * `vi.stubGlobal('fetch')` is not an option here — probed in this environment,
 * `axios.defaults.adapter` is `["xhr","http","fetch"]` and jsdom ships
 * `XMLHttpRequest`, so the xhr adapter wins and `fetch` is never consulted.
 */
export interface Call {
  url: string;
  method: string;
  bearer: string | undefined;
  body: unknown;
}

export interface Scripted {
  status: number;
  data?: unknown;
}

/** `attempt` counts calls to the same method+url, so a script can 401 once and succeed next. */
export type Script = (call: Call, attempt: number) => Scripted;

export function axiosHarness(script: Script) {
  const calls: Call[] = [];

  const adapter = async (config: InternalAxiosRequestConfig): Promise<AxiosResponse> => {
    const call: Call = {
      url: config.url ?? '',
      method: (config.method ?? 'get').toLowerCase(),
      bearer: config.headers?.Authorization as string | undefined,
      body: config.data ? JSON.parse(String(config.data)) : undefined,
    };
    calls.push(call);
    const attempt = calls.filter((c) => c.url === call.url && c.method === call.method).length;
    const res = script(call, attempt);

    if (res.status >= 200 && res.status < 300) {
      return { status: res.status, statusText: 'OK', data: res.data ?? {}, headers: {}, config };
    }
    throw new AxiosError('request refused', 'ERR_BAD_REQUEST', config, {}, {
      status: res.status,
      statusText: '',
      data: res.data ?? {},
      headers: {},
      config,
    } as AxiosResponse);
  };

  return { calls, adapter };
}

/** The server's real refusal for an anonymous or expired call (api/middleware.go). */
export const UNAUTHORIZED: Scripted = { status: 401, data: { error: 'access token is not valid' } };
