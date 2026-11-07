import os


def handler(event):
    greeting = os.environ.get("GREETING", "Hello")
    return {"message": f"{greeting}, {event.get('name', 'World')}!"}
