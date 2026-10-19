FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    OBSAI_BIND_HOST=0.0.0.0 \
    OBSAI_BIND_PORT=8080

WORKDIR /app
COPY pyproject.toml ./
COPY obsai ./obsai

RUN useradd --create-home --uid 10001 obsai \
    && mkdir -p /state \
    && chown -R obsai:obsai /app /state

USER obsai
EXPOSE 8080
VOLUME ["/state"]
CMD ["python", "-m", "obsai"]
