/**
 * Shared pino logger for the BFF. Level is set by LOG_LEVEL (default `info`);
 * set LOG_PRETTY=true for human-readable single-line output in local dev.
 */
import pino from 'pino'

const level = process.env.LOG_LEVEL ?? 'info'

export const logger = pino(
  process.env.LOG_PRETTY === 'true'
    ? { level, transport: { target: 'pino-pretty', options: { colorize: true, singleLine: true } } }
    : { level }
)
