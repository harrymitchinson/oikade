/** Structured logging. Fields must never carry secrets. */
export interface Logger {
  info(message: string, fields?: Record<string, unknown>): void;
  warn(message: string, fields?: Record<string, unknown>): void;
  error(message: string, fields?: Record<string, unknown>): void;
}

/** Writes one JSON object per line, info to stdout and the rest to stderr. */
export const jsonLogger: Logger = {
  info: (message, fields) => write("info", message, fields),
  warn: (message, fields) => write("warn", message, fields),
  error: (message, fields) => write("error", message, fields),
};

function write(
  level: string,
  message: string,
  fields: Record<string, unknown> = {}
): void {
  const line = JSON.stringify({
    time: new Date().toISOString(),
    level,
    msg: message,
    ...fields,
  });
  if (level === "info") {
    process.stdout.write(line + "\n");
  } else {
    process.stderr.write(line + "\n");
  }
}
