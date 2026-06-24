/** Base URL of the conductor API gateway every BFF request is proxied to. Defaults to the local compose endpoint. */
export const CONDUCTOR_URL = process.env.CONDUCTOR_URL ?? 'http://localhost:8080';
