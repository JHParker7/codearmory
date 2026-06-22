import pino from 'pino'

const level = process.env.LOG_LEVEL ?? 'info'

export const logger = pino(
  process.env.LOG_PRETTY === 'true'
    ? { level, transport: { target: 'pino-pretty', options: { colorize: true, singleLine: true } } }
    : { level }
)
