# Dashboard Dockerfile for OSS self-host deployments.
#
# Build: docker build -f Dockerfile -t pandastack/dashboard:dev ./dashboard
# Run:   docker run -p 3000:3000 -e NEXT_PUBLIC_PANDASTACK_API=http://api:8080 \
#                  pandastack/dashboard:dev
#
# Self-hosters: Supabase auth is the default. To run without Supabase, set
# NEXT_PUBLIC_PANDASTACK_AUTH=token at build time (uses the api's token-file
# auth instead of JWT). Phase 1.5 will make this a first-class option.

FROM node:20-alpine AS deps
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci --no-audit --no-fund

FROM node:20-alpine AS build
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
ARG NEXT_PUBLIC_PANDASTACK_API=http://localhost:8080
ENV NEXT_PUBLIC_PANDASTACK_API=$NEXT_PUBLIC_PANDASTACK_API
RUN npm run build

FROM node:20-alpine AS run
WORKDIR /app
ENV NODE_ENV=production
RUN addgroup -S app && adduser -S app -G app
COPY --from=build /app/.next/standalone ./
COPY --from=build /app/.next/static ./.next/static
COPY --from=build /app/public ./public
USER app
EXPOSE 3000
ENV PORT=3000 HOSTNAME=0.0.0.0
CMD ["node", "server.js"]
