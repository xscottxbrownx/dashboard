export const API_URL = env.API_URL || "http://localhost:8081"
export const PLACEHOLDER_DOCS_URL = "https://docs.ticketsbot.cloud/setup/placeholders.html"
export const STRIPE = {
    publishKey: env.STRIPE_PUBLISH_KEY || "pk_test_51QpuwdBXCkKxYGKwRUAhyc5RO8H9JRaIUOy8Apg85GfbUdtcyfo5P34yFpuYds13qZ8aMUtwhKesMZDA1F884A8u00fDXFAgHf",
    checkoutUrl: env.STRIPE_PORTAL_URL || "https://billing.stripe.com/p/login/test_7sI189bPP9QBbDy9AA"
}

export const OAUTH = {
    clientId: env.CLIENT_ID || "1325579039888511056",
    redirectUri: env.REDIRECT_URI || "http://localhost:5000/callback"
}
