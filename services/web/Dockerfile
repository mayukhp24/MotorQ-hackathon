# Build context: services/web
FROM node:22-alpine AS build
WORKDIR /src
COPY package.json package-lock.json ./
# Optional build secret `extra_ca` (TLS-inspecting proxy) is used in the same
# step as the download, so it is never written to a layer or cached stale.
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then export NODE_EXTRA_CA_CERTS=/run/secrets/extra_ca; fi; \
    npm ci --no-audit --no-fund
COPY . .
RUN npm run build

FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY security-headers.conf /etc/nginx/snippets/security-headers.conf
COPY --from=build /src/dist /usr/share/nginx/html
RUN chown -R nginx:nginx /var/cache/nginx /usr/share/nginx/html && \
    sed -i 's/^user .*;//' /etc/nginx/nginx.conf && \
    sed -i 's#pid .*;#pid /tmp/nginx.pid;#' /etc/nginx/nginx.conf && touch /tmp/nginx.pid && chown nginx /tmp/nginx.pid
USER nginx
EXPOSE 8080
